// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// https://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#ifndef BUILDXL_CONFIG_H_
#define BUILDXL_CONFIG_H_

#include <charconv>
#include <fstream>
#include <functional>
#include <limits>
#include <set>
#include <string>
#include <vector>

#include "loc.h"
#include "log.h"

// A caller-owned table prefix and explicit process fields, not graph policy.
class BuildXLConfig {
 public:
  struct Token {
    std::string value;
    bool quoted = false;
  };
  struct Row {
    std::vector<Token> tokens;
    Loc loc;
  };
  std::vector<std::string> paths;
  std::vector<std::string> strings;
  std::vector<std::vector<size_t>> sets;
  std::vector<Row> process;
  std::set<std::string> inherited_environment;
  std::set<std::string> passthrough_environment;

  static bool IsTempName(std::string_view name) {
    return name == "TEMP" || name == "TMP" || name == "TMPDIR";
  }

  void Load(const char* filename,
            const std::function<std::string(std::string_view)>& normalize) {
    if (!filename) {
      return;
    }
    std::ifstream in(filename, std::ios::binary);
    if (!in) {
      ERROR("Cannot read BuildXL config: %s", filename);
    }
    std::string line;
    int number = 0;
    bool header = false;
    bool in_process = false;
    while (std::getline(in, line)) {
      Loc loc(filename, ++number);
      auto tokens = Tokenize(line, loc);
      if (tokens.empty()) {
        continue;
      }
      if (!header) {
        if (tokens.size() != 2 || tokens[0].value != "buildxlConfig" ||
            tokens[1].value != "1" || tokens[0].quoted || tokens[1].quoted) {
          ERROR_LOC(loc, "BuildXL config expects 'buildxlConfig 1'");
        }
        header = true;
        continue;
      }
      const auto& kind = tokens[0].value;
      if (tokens[0].quoted) {
        ERROR_LOC(loc, "BuildXL config field names must be bare");
      }
      if (line.front() == ' ' || line.front() == '\t') {
        if (!in_process) {
          ERROR_LOC(loc, "BuildXL config fields require a process section");
        }
        ValidateRow(tokens, loc);
        process.push_back({std::move(tokens), loc});
        continue;
      }
      if (kind == "process" && tokens.size() == 1 && !in_process) {
        in_process = true;
      } else if (!in_process && (kind == "path" || kind == "string")) {
        auto& table = kind == "path" ? paths : strings;
        if (tokens.size() != 3 || tokens[1].quoted ||
            Id(tokens[1].value, loc) != table.size() || !tokens[2].quoted) {
          ERROR_LOC(loc,
                    "BuildXL config requires dense table IDs and one literal");
        }
        table.push_back(kind == "path" ? normalize(tokens[2].value)
                                       : tokens[2].value);
      } else if (!in_process && kind == "set") {
        if (tokens.size() < 2 || tokens[1].quoted ||
            Id(tokens[1].value, loc) != sets.size()) {
          ERROR_LOC(loc, "BuildXL config requires dense set IDs");
        }
        std::vector<size_t> members;
        for (size_t i = 2; i < tokens.size(); ++i) {
          members.push_back(Reference(tokens[i], 'p', loc));
        }
        sets.push_back(std::move(members));
      } else if (!in_process && (kind == "inheritEnvironment" ||
                                 kind == "passthroughEnvironment")) {
        auto& names = kind == "inheritEnvironment" ? inherited_environment
                                                   : passthrough_environment;
        for (size_t i = 1; i < tokens.size(); ++i) {
          const auto& name = tokens[i].value;
          if (name.empty() || name.find('=') != std::string::npos ||
              name.find_first_of("$@\n\r\t ") != std::string::npos ||
              !names.insert(name).second) {
            ERROR_LOC(loc, "BuildXL config invalid/duplicate environment name");
          }
          if (kind == "inheritEnvironment" && IsTempName(name)) {
            ERROR_LOC(loc, "BuildXL config cannot inherit TEMP/TMP/TMPDIR");
          }
        }
      } else {
        ERROR_LOC(loc, "BuildXL config unsupported record: %s", kind.c_str());
      }
    }
    if (!in.eof()) {
      ERROR("Cannot read BuildXL config: %s", filename);
    }
    if (!header) {
      ERROR("BuildXL config missing header: %s", filename);
    }
  }

  bool HasOption(std::string_view option) const {
    return options_.count(std::string(option));
  }

  template <typename Owners>
  void ValidateProducts(const Owners& owners, size_t action_count) const {
    if (action_count > 1 && (explicit_temp_ || exclusive_output_)) {
      ERROR(
          "BuildXL config explicit temp/exclusive output directories cannot be "
          "shared by multiple emitted processes; use automatic temp/shared "
          "outputs");
    }
    for (const auto& input : inputs_) {
      if (owners.count(input.first)) {
        ERROR_LOC(input.second,
                  "BuildXL config input is a product of this fragment "
                  "(self/forward reference): %s",
                  input.first.c_str());
      }
      for (const auto& root : writable_roots_) {
        if (Within(input.first, root)) {
          ERROR_LOC(input.second,
                    "BuildXL config input refers to its own writable root: %s",
                    input.first.c_str());
        }
      }
    }
    for (const auto& input : input_directories_) {
      for (const auto& root : writable_roots_) {
        if (Within(input.first, root) || Within(root, input.first)) {
          ERROR_LOC(input.second,
                    "BuildXL config inputDirectory overlaps its own writable "
                    "root: %s",
                    input.first.c_str());
        }
      }
    }
  }

 private:
  static bool Within(const std::string& path, const std::string& root) {
    return path == root || (path.size() > root.size() &&
                            path.compare(0, root.size(), root) == 0 &&
                            (root.back() == '/' || path[root.size()] == '/'));
  }

  static size_t Id(std::string_view value, const Loc& loc) {
    size_t result = 0;
    const auto parsed =
        std::from_chars(value.data(), value.data() + value.size(), result);
    if (value.empty() || (value.size() > 1 && value.front() == '0') ||
        parsed.ec != std::errc() || parsed.ptr != value.data() + value.size()) {
      ERROR_LOC(loc, "BuildXL config invalid decimal ID/integer: %.*s",
                static_cast<int>(value.size()), value.data());
    }
    return result;
  }

  size_t Reference(const Token& token, char kind, const Loc& loc) const {
    if (token.quoted || token.value.size() < 3 || token.value[0] != '$' ||
        token.value[1] != kind) {
      ERROR_LOC(loc, "BuildXL config expects a $%c table reference", kind);
    }
    const size_t id = Id(std::string_view(token.value).substr(2), loc);
    if (id >= (kind == 'p' ? paths.size() : strings.size())) {
      ERROR_LOC(loc, "BuildXL config undefined table reference: %s",
                token.value.c_str());
    }
    return id;
  }

  void ValidateRow(const std::vector<Token>& row, const Loc& loc) {
    const auto& field = row[0].value;
    auto arity = [&](size_t min, size_t max) {
      if (row.size() < min + 1 || row.size() > max + 1) {
        ERROR_LOC(loc, "BuildXL config invalid arity for %s", field.c_str());
      }
    };
    auto literal = [&](size_t index) -> const std::string& {
      if (!row[index].value.empty() &&
          (row[index].value[0] == '$' || row[index].value[0] == '@') &&
          !row[index].quoted) {
        ERROR_LOC(loc, "BuildXL config %s expects a literal", field.c_str());
      }
      return row[index].value;
    };
    if (field == "input" || field == "orderOnly") {
      arity(1, std::numeric_limits<size_t>::max() - 1);
      for (size_t i = 1; i < row.size(); ++i) {
        if (!row[i].quoted && row[i].value.front() == '@') {
          size_t id = Id(std::string_view(row[i].value).substr(1), loc);
          if (id >= sets.size()) {
            ERROR_LOC(loc, "BuildXL config undefined dependency set");
          }
          for (size_t path : sets[id]) {
            inputs_.emplace_back(paths[path], loc);
          }
        } else {
          inputs_.emplace_back(paths[Reference(row[i], 'p', loc)], loc);
        }
      }
    } else if (field == "inputDirectory" || field == "outputDirectory") {
      arity(1, 2);
      const auto& path = paths[Reference(row[1], 'p', loc)];
      if (field == "inputDirectory") {
        input_directories_.emplace_back(path, loc);
        if (row.size() == 3 && literal(2).empty()) {
          ERROR_LOC(loc, "BuildXL config empty directory producer");
        }
      } else {
        if (!writable_roots_.insert(path).second) {
          ERROR_LOC(loc, "BuildXL config duplicate outputDirectory: %s",
                    path.c_str());
        }
        if (row.size() == 3) {
          const auto& kind = literal(2);
          if (kind != "shared" && kind != "exclusive") {
            ERROR_LOC(loc, "BuildXL config unknown outputDirectory kind");
          }
          exclusive_output_ |= kind == "exclusive";
        }
      }
    } else if (field == "tempDirectory") {
      arity(1, 1);
      Reference(row[1], 'p', loc);
      if (explicit_temp_ || automatic_temp_) {
        ERROR_LOC(loc, "BuildXL config conflicting temp settings");
      }
      explicit_temp_ = true;
    } else if (field == "enableTempDirectory") {
      arity(1, 1);
      const auto& value = literal(1);
      if ((value != "true" && value != "false") || seen_automatic_temp_ ||
          (value == "true" && explicit_temp_)) {
        ERROR_LOC(loc,
                  "BuildXL config conflicting/invalid automatic temp setting");
      }
      seen_automatic_temp_ = true;
      automatic_temp_ = value == "true";
    } else if (field == "semaphore") {
      arity(3, 3);
      const auto& name = strings[Reference(row[1], 's', loc)];
      size_t limit = Id(literal(2), loc);
      size_t increment = Id(literal(3), loc);
      if (name.empty() || !semaphores_.insert(name).second || limit == 0 ||
          limit > INT32_MAX || increment == 0 || increment > limit) {
        ERROR_LOC(loc, "BuildXL config invalid/duplicate semaphore");
      }
    } else if (field == "option") {
      arity(1, 1);
      static const std::set<std::string> supported = {
          "keepOutputsWritable",
          "requiresAdmin",
          "isLight",
          "scanForBuildxlHints",
          "allowUndeclaredSourceReads",
          "preservePathSetCasing",
          "enforceWeakFingerprintAugmentation",
          "disableCacheLookup",
          "uncancellable",
          "mustRunOnOrchestrator",
          "writingToStandardErrorFailsExecution",
          "dependsOnCurrentOs",
          "dependsOnCurrentHostOsDirectories",
          "dependsOnWindowsAppData",
          "unsafeHasUntrackedChildProcesses",
          "unsafeAllowPreservedOutputs",
          "unsafeIncrementalTool",
          "unsafeSkipGlobalDependencies",
          "unsafeTrustStaticallyDeclaredAccesses",
          "unsafeDisableFullReparsePointResolving",
          "unsafeDisableSandboxing"};
      const auto& option = literal(1);
      if (!supported.count(option) || !options_.insert(option).second) {
        ERROR_LOC(loc, "BuildXL config unknown/duplicate process option: %s",
                  option.c_str());
      }
    } else {
      ERROR_LOC(loc, "BuildXL config unsupported process field: %s",
                field.c_str());
    }
  }

  static std::vector<Token> Tokenize(std::string_view line, const Loc& loc) {
    std::vector<Token> tokens;
    size_t i = line.find_first_not_of(" \t\r");
    if (i == std::string_view::npos || line[i] == '#') {
      return tokens;
    }
    while (i < line.size()) {
      Token token;
      token.quoted = line[i] == '"';
      if (token.quoted) {
        ++i;
        while (i < line.size() && line[i] != '"') {
          char c = line[i++];
          if (c == '\\') {
            if (i == line.size()) {
              ERROR_LOC(loc, "BuildXL config incomplete escape");
            }
            switch (line[i++]) {
              case '\\':
                c = '\\';
                break;
              case '"':
                c = '"';
                break;
              case 'n':
                c = '\n';
                break;
              case 'r':
                c = '\r';
                break;
              case 't':
                c = '\t';
                break;
              default:
                ERROR_LOC(
                    loc,
                    "BuildXL config unsupported escape; use UTF-8 literals");
            }
          } else if (static_cast<unsigned char>(c) < 32 || c == 127) {
            ERROR_LOC(loc, "BuildXL config control characters must be escaped");
          }
          token.value += c;
        }
        if (i == line.size()) {
          ERROR_LOC(loc, "BuildXL config unterminated literal");
        }
        ++i;
        if (i < line.size() && line[i] != ' ' && line[i] != '\t' &&
            line[i] != '\r') {
          ERROR_LOC(loc, "BuildXL config missing token separator");
        }
      } else {
        size_t start = i;
        while (i < line.size() && line[i] != ' ' && line[i] != '\t' &&
               line[i] != '\r') {
          if (line[i] == '"' || line[i] == '\\' ||
              static_cast<unsigned char>(line[i]) < 32) {
            ERROR_LOC(loc, "BuildXL config invalid bare token");
          }
          ++i;
        }
        token.value = line.substr(start, i - start);
      }
      if (token.value.find('\0') != std::string::npos) {
        ERROR_LOC(loc, "BuildXL config NUL is not allowed");
      }
      tokens.push_back(std::move(token));
      i = line.find_first_not_of(" \t\r", i);
      if (i == std::string_view::npos) {
        break;
      }
    }
    return tokens;
  }

  bool explicit_temp_ = false;
  bool automatic_temp_ = false;
  bool seen_automatic_temp_ = false;
  bool exclusive_output_ = false;
  std::set<std::string> semaphores_;
  std::set<std::string> options_;
  std::set<std::string> writable_roots_;
  std::vector<std::pair<std::string, Loc>> inputs_;
  std::vector<std::pair<std::string, Loc>> input_directories_;
};

#endif  // BUILDXL_CONFIG_H_
