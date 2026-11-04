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

// +build ignore

// BuildXL-specific export through the DirectGraph textual interface.
#include "buildxl.h"

#include <unistd.h>

#include <filesystem>
#include <fstream>
#include <functional>
#include <map>
#include <set>
#include <unordered_map>

#include "buildxl_config.h"
#include "command.h"
#include "eval.h"
#include "flags.h"
#include "loc.h"
#include "log.h"
#include "rule.h"
#include "stringprintf.h"
#include "var.h"

extern char** environ;

namespace {

bool IsValidUtf8(std::string_view value) {
  for (size_t i = 0; i < value.size(); ++i) {
    const unsigned char c = value[i];
    if (c < 0x80) {
      continue;
    }
    unsigned codepoint;
    unsigned minimum;
    size_t continuation;
    if (c >= 0xc2 && c <= 0xdf) {
      codepoint = c & 0x1f;
      minimum = 0x80;
      continuation = 1;
    } else if (c >= 0xe0 && c <= 0xef) {
      codepoint = c & 0x0f;
      minimum = 0x800;
      continuation = 2;
    } else if (c >= 0xf0 && c <= 0xf4) {
      codepoint = c & 0x07;
      minimum = 0x10000;
      continuation = 3;
    } else {
      return false;
    }
    if (continuation >= value.size() - i) {
      return false;
    }
    for (size_t j = 0; j < continuation; ++j) {
      const unsigned char next = value[++i];
      if ((next & 0xc0) != 0x80) {
        return false;
      }
      codepoint = (codepoint << 6) | (next & 0x3f);
    }
    if (codepoint < minimum || codepoint > 0x10ffff ||
        (codepoint >= 0xd800 && codepoint <= 0xdfff)) {
      return false;
    }
  }
  return true;
}

void Literal(std::ostream& out, std::string_view s) {
  out << '"';
  for (size_t i = 0; i < s.size(); ++i) {
    const unsigned char c = s[i];
    // C1 controls are two-byte UTF-8 sequences, unlike the ASCII controls
    // below.
    if (c == 0xc2 && i + 1 < s.size()) {
      const unsigned char next = s[i + 1];
      if (next >= 0x80 && next <= 0x9f) {
        out << StringPrintf("\\u%04x", next);
        ++i;
        continue;
      }
    }
    switch (c) {
      case '"':
        out << "\\\"";
        break;
      case '\\':
        out << "\\\\";
        break;
      case '\n':
        out << "\\n";
        break;
      case '\r':
        out << "\\r";
        break;
      case '\t':
        out << "\\t";
        break;
      default:
        if (c < 32 || c == 127) {
          out << StringPrintf("\\u%04x", c);
        } else {
          out << c;
        }
    }
  }
  out << '"';
}

std::string Quote(std::string_view s) {
  return QuoteShellLiteral(s);
}

// SHELL may include options. Decode literal shell words without executing
// expansions or silently treating shell syntax as executable arguments.
std::vector<std::string> ShellWords(std::string_view value, const Loc& loc) {
  std::vector<std::string> words;
  std::string word;
  char quote = 0;
  bool started = false;
  for (size_t i = 0; i < value.size(); ++i) {
    const char c = value[i];
    if (c == '\0' || c == '\n' || c == '\r') {
      ERROR_LOC(loc, "BuildXL unsupported SHELL syntax");
    }
    if (quote != '\'' && c == '\\') {
      if (++i == value.size()) {
        ERROR_LOC(loc, "BuildXL unterminated SHELL escape");
      }
      const char next = value[i];
      if (next == '\0' || next == '\n' || next == '\r') {
        ERROR_LOC(loc, "BuildXL unsupported SHELL syntax");
      }
      if (quote == '"' && next != '$' && next != '`' && next != '"' &&
          next != '\\') {
        word += '\\';
      }
      word += next;
      started = true;
    } else if (quote && c == quote) {
      quote = 0;
    } else if (!quote && (c == '\'' || c == '"')) {
      quote = c;
      started = true;
    } else if (!quote && (c == ' ' || c == '\t')) {
      if (started) {
        words.push_back(std::move(word));
        word.clear();
        started = false;
      }
    } else {
      if ((quote != '\'' && (c == '$' || c == '`')) ||
          (!quote && std::string_view("|&;<>()*?[]~#").find(c) !=
                         std::string_view::npos)) {
        ERROR_LOC(loc, "BuildXL unsupported SHELL syntax");
      }
      word += c;
      started = true;
    }
  }
  if (quote) {
    ERROR_LOC(loc, "BuildXL unterminated SHELL quote");
  }
  if (started) {
    words.push_back(std::move(word));
  }
  return words;
}

// Lossless string factoring, not shell tokenization: quotes stay literal
// chunks.
std::vector<std::string_view> StringParts(std::string_view value) {
  std::vector<std::string_view> parts;
  size_t start = 0;
  auto whitespace = [](char c) {
    return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' ||
           c == '\v';
  };
  auto quote = [](char c) { return c == '"' || c == '\'' || c == '`'; };
  for (size_t i = 1; i < value.size(); ++i) {
    if (quote(value[i]) || quote(value[i - 1]) ||
        whitespace(value[i]) != whitespace(value[i - 1])) {
      parts.push_back(value.substr(start, i - start));
      start = i;
    }
  }
  parts.push_back(value.substr(start));
  return parts;
}

std::string ActionNameHash(std::string_view s) {
  uint64_t h = UINT64_C(14695981039346656037);
  for (unsigned char c : s) {
    h ^= c;
    h *= UINT64_C(1099511628211);
  }
  return StringPrintf("%016llx", static_cast<unsigned long long>(h));
}

enum class VisitState { Unvisited, Visiting, Complete };

struct Action {
  struct Script {
    std::string name;
    std::string_view path;
    std::string content;
  };
  const DepNode* node;
  std::string name;
  std::string_view shell;
  std::vector<std::string> shell_arguments;
  std::string flags;
  std::map<std::string_view, std::string_view> environment;
  std::vector<Command> commands;
  std::vector<std::string_view> outputs;
  std::vector<std::string_view> normal;
  std::vector<std::string_view> ordered;
  std::string script;
  std::vector<Script> scripts;
  std::vector<std::string> arguments;
  std::vector<std::string_view> script_inputs;
};

class BuildXLGenerator {
 public:
  BuildXLGenerator(Evaluator* ev, const char* filename)
      : path_pool_(),
        ev_(ev),
        ce_(ev),
        root_(InternPath(std::filesystem::current_path().string())),
        filename_(Path(filename)),
        base_environment_(BaseEnvironment()) {}

  void Generate(const std::vector<NamedDepNode>& roots) {
    config_.Load(g_flags.buildxl_config, [&](std::string_view value) {
      std::filesystem::path path(Path(value));
      if (path != path.root_path() && path.filename().empty()) {
        path = path.parent_path();
      }
      return path.string();
    });
    if (g_flags.buildxl_config) {
      sources_.emplace(Path(g_flags.buildxl_config), Loc());
    }
    for (size_t id = 0; id < config_.paths.size(); ++id) {
      const auto& value = InternPath(config_.paths[id]);
      if (!IsValidUtf8(value)) {
        ERROR("BuildXL config text requires valid UTF-8");
      }
      paths_.emplace(value, id);
    }
    for (size_t id = 0; id < config_.strings.size(); ++id) {
      if (!IsValidUtf8(config_.strings[id])) {
        ERROR("BuildXL config text requires valid UTF-8");
      }
      strings_.emplace(config_.strings[id], id);
    }
    for (const auto& row : config_.process) {
      for (const auto& token : row.tokens) {
        if (!IsValidUtf8(token.value)) {
          ERROR_LOC(row.loc, "BuildXL config text requires valid UTF-8");
        }
      }
      for (const auto* names :
           {&config_.inherited_environment, &config_.passthrough_environment}) {
        for (const auto& name : *names) {
          if (!IsValidUtf8(name)) {
            ERROR("BuildXL config environment names require valid UTF-8");
          }
        }
      }
    }
    path_id_bias_ = config_.paths.size() - paths_.size();
    string_id_bias_ = config_.strings.size() - strings_.size();
    ValidateConfiguration();
    ev_->set_avoid_io(true);
    for (const auto& p : roots) {
      Visit(p.second, p.second->loc);
    }

    ResolveDependencies(roots);
    for (Action& a : actions_) {
      Lower(a);
    }
    config_.ValidateProducts(owners_, actions_.size());
    for (Action& a : actions_) {
      InternValues(a);
    }
    const auto owner = owners_.find(filename_);
    const auto source = sources_.find(filename_);
    if (owner != owners_.end() || source != sources_.end()) {
      ERROR_LOC(owner != owners_.end() ? owner->second : source->second,
                "BuildXL fragment filename overlaps a graph artifact: %s",
                filename_.c_str());
    }
    ev_->set_avoid_io(false);
    WriteDirectGraph();
  }

 private:
  void ValidateConfiguration() const {
    if (g_flags.detect_depfiles || g_flags.use_ninja_validations ||
        g_flags.default_pool || g_flags.detect_android_echo) {
      ERROR(
          "BuildXL does not support Ninja depfile, validation, pool or "
          "Android command rewriting options");
    }
    for (const Rule* r : ev_->rules()) {
      if (r->is_double_colon) {
        ERROR_LOC(r->loc, "BuildXL does not support double-colon rules");
      }
    }
  }

  const std::string& InternPath(std::string value) {
    return *path_pool_.insert(std::move(value)).first;
  }

  const std::string& Path(std::string_view value,
                          const Loc& loc = Loc(),
                          bool artifact = true) {
    if (value.empty() || value.find('\0') != std::string_view::npos) {
      ERROR_LOC(loc, "BuildXL: empty or NUL-containing path");
    }
    std::filesystem::path p{std::string(value)};
    if (!p.is_absolute()) {
      p = std::filesystem::path(root_) / p;
    }
    if (artifact) {
      std::filesystem::path prefix;
      for (const auto& component : p) {
        prefix /= component;
        std::error_code error;
        if (std::filesystem::is_symlink(
                std::filesystem::symlink_status(prefix, error))) {
          ERROR_LOC(loc, "BuildXL does not support symlink artifact paths: %s",
                    prefix.c_str());
        }
      }
    }
    return InternPath(p.lexically_normal().string());
  }

  void Claim(std::string_view path, const Loc& loc) {
    if (path == root_ || std::filesystem::path(path).filename().empty() ||
        std::filesystem::is_directory(path)) {
      ERROR_LOC(loc, "BuildXL supports file outputs only: %.*s", SPF(path));
    }
    if (!owners_.emplace(path, loc).second) {
      ERROR_LOC(loc, "BuildXL duplicate output ownership: %.*s", SPF(path));
    }
    for (std::filesystem::path parent =
             std::filesystem::path(path).parent_path();
         !parent.empty() && parent != parent.root_path();
         parent = parent.parent_path()) {
      if (owners_.count(parent.string())) {
        ERROR_LOC(loc, "BuildXL file/directory output overlap: %.*s",
                  SPF(path));
      }
    }
    for (const auto& p : owners_) {
      if (p.first.size() > path.size() &&
          p.first.compare(0, path.size(), path) == 0 &&
          p.first[path.size()] == '/') {
        ERROR_LOC(loc, "BuildXL file/directory output overlap: %.*s",
                  SPF(path));
      }
    }
  }

  static std::map<std::string, std::string> BaseEnvironment() {
    std::map<std::string, std::string> env;
    for (char** p = environ; *p; ++p) {
      const std::string_view s(*p);
      const size_t equals = s.find('=');
      if (equals != std::string::npos) {
        env[std::string(s.substr(0, equals))] = s.substr(equals + 1);
      }
    }
    return env;
  }

  std::map<std::string_view, std::string_view> Environment(const DepNode& n) {
    std::map<std::string_view, std::string_view> env;
    for (const auto& p : base_environment_) {
      if (!BuildXLConfig::IsTempName(p.first) &&
          (!g_flags.buildxl_config ||
           config_.inherited_environment.count(p.first)) &&
          !config_.passthrough_environment.count(p.first)) {
        env.emplace(p.first, p.second);
      }
    }
    ev_->set_current_scope(n.rule_vars);
    for (const auto& p : ev_->exports()) {
      if (config_.passthrough_environment.count(std::string(p.first.str()))) {
        continue;
      }
      if (p.second) {
        const std::string_view name = p.first.str();
        std::string value = ev_->EvalVar(p.first);
        if (name.empty() || name.find('=') != std::string::npos ||
            value.find('\0') != std::string::npos) {
          ERROR_LOC(n.loc, "BuildXL invalid exported environment variable");
        }
        env[name] = *environment_values_.insert(std::move(value)).first;
      } else {
        env.erase(p.first.str());
      }
    }
    ev_->set_current_scope(nullptr);
    if (!ev_->delayed_output_commands().empty()) {
      ERROR_LOC(n.loc,
                "BuildXL does not support delayed effects in exported "
                "environment");
    }
    return env;
  }

  // Expand selected targets in dependency post-order, appending command-bearing
  // nodes to actions_ after their prerequisites.
  void Visit(const DepNode* n, const Loc& parent_loc) {
    const Loc& loc = n->loc.filename ? n->loc : parent_loc;
    VisitState& state = states_[n];
    if (state == VisitState::Visiting) {
      ERROR_LOC(loc, "BuildXL dependency cycle at %s", n->output.c_str());
    }
    if (state == VisitState::Complete) {
      return;
    }
    state = VisitState::Visiting;
    Action a = EvaluateAction(n, loc);
    for (const auto& p : n->deps) {
      Visit(p.second, loc);
    }
    for (const auto& p : n->order_onlys) {
      Visit(p.second, loc);
    }
    states_[n] = VisitState::Complete;
    if (!a.commands.empty()) {
      actions_.push_back(std::move(a));
    }
  }

  Action EvaluateAction(const DepNode* n, const Loc& loc) {
    if (n->depfile_var || !n->validations.empty() || n->ninja_pool_var ||
        n->tags_var) {
      ERROR_LOC(loc,
                "BuildXL unsupported depfile, validations, pool or tags on %s",
                n->output.c_str());
    }
    Action a;
    a.node = n;
    a.name = "action_" + ActionNameHash(Path(n->output.str(), loc));
    a.commands = ce_.Eval(*n);
    if (!a.commands.empty()) {
      if (n->is_phony) {
        ERROR_LOC(loc, "BuildXL does not support commandful phony target %s",
                  n->output.c_str());
      }
      commandful_.insert(n);
      ev_->set_current_scope(n->rule_vars);
      if (ev_->LookupVar(Intern(".SHELLFLAGS"))->IsDefined()) {
        ERROR_LOC(loc, "BuildXL does not support custom .SHELLFLAGS");
      }
      std::vector<std::string> shell = ShellWords(ev_->GetShell(), loc);
      a.flags = ev_->GetShellFlag();
      ev_->set_current_scope(nullptr);
      if (shell.empty() || shell.front().empty() ||
          shell.front().front() != '/') {
        ERROR_LOC(loc, "BuildXL SHELL must be an absolute executable path");
      }
      a.shell = Path(shell.front(), loc, false);
      a.shell_arguments.assign(shell.begin() + 1, shell.end());
      a.environment = Environment(*n);
      a.outputs.push_back(Path(n->output.str(), loc));
      for (Symbol s : n->implicit_outputs) {
        a.outputs.push_back(Path(s.str(), loc));
      }
      for (std::string_view p : a.outputs) {
        Claim(p, loc);
      }
      if (!names_.insert(a.name).second) {
        ERROR_LOC(loc, "BuildXL action name collision");
      }
    }
    return a;
  }

  void ResolveDependencies(const std::vector<NamedDepNode>& roots) {
    for (Action& a : actions_) {
      for (const auto& p : a.node->deps) {
        Flatten(p, false, &a.normal, &a.ordered, a.node->loc);
      }
      for (const auto& p : a.node->order_onlys) {
        Flatten(p, true, &a.normal, &a.ordered, a.node->loc);
      }
      CountSet(a.normal);
      CountSet(a.ordered);
    }
    SortActions();
    for (const auto& p : roots) {
      if (commandful_.count(p.second) &&
          !owners_.count(Path(p.first.str(), p.second->loc))) {
        ERROR_LOC(
            p.second->loc,
            "BuildXL requested target %s has no declared output ownership",
            p.first.c_str());
      }
    }
  }

  void Flatten(const NamedDepNode& p,
               bool ordered,
               std::vector<std::string_view>* normal,
               std::vector<std::string_view>* order,
               const Loc& loc) {
    const DepNode* n = p.second;
    const std::string_view path = Path(p.first.str(), loc);
    const auto owner = owners_.find(path);
    if (owner == owners_.end() && commandful_.count(n)) {
      ERROR_LOC(n->loc,
                "BuildXL prerequisite %s has no declared output ownership",
                p.first.c_str());
    }
    // Like Kati's Ninja backend, evaluated commandless rules are phony edges.
    if (owner == owners_.end() && (n->is_phony || n->has_rule)) {
      for (const auto& child : n->deps) {
        Flatten(child, ordered, normal, order, loc);
      }
      for (const auto& child : n->order_onlys) {
        Flatten(child, true, normal, order, loc);
      }
      return;
    }
    if (owner == owners_.end()) {
      if (std::filesystem::is_directory(path)) {
        ERROR_LOC(loc, "BuildXL supports file prerequisites only: %.*s",
                  SPF(path));
      }
      sources_.emplace(path, loc);
    }
    (ordered ? order : normal)->push_back(path);
  }

  void CountSet(const std::vector<std::string_view>& list) {
    if (!list.empty()) {
      ++set_counts_[list];
    }
  }

  // Normalized paths can reveal producer edges absent from the DepNode
  // traversal. Re-sort by artifact ownership, also detecting cycles introduced
  // by aliases.
  void SortActions() {
    std::map<std::string_view, size_t> producers;
    for (size_t i = 0; i < actions_.size(); ++i) {
      const Action& a = actions_[i];
      for (std::string_view p : a.outputs) {
        producers[p] = i;
      }
    }
    std::vector<VisitState> states(actions_.size(), VisitState::Unvisited);
    std::vector<size_t> order;
    order.reserve(actions_.size());

    std::function<void(size_t)> visit = [&](size_t i) {
      const Action& a = actions_[i];
      if (states[i] == VisitState::Visiting) {
        ERROR_LOC(a.node->loc, "BuildXL normalized artifact dependency cycle");
      }
      if (states[i] == VisitState::Complete) {
        return;
      }
      states[i] = VisitState::Visiting;
      for (const auto* list : {&a.normal, &a.ordered}) {
        for (std::string_view ref : *list) {
          auto p = producers.find(ref);
          if (p != producers.end()) {
            visit(p->second);
          }
        }
      }
      states[i] = VisitState::Complete;
      order.push_back(i);
    };

    for (size_t i = 0; i < actions_.size(); ++i) {
      visit(i);
    }

    std::vector<Action> sorted;
    sorted.reserve(actions_.size());
    for (size_t i : order) {
      sorted.push_back(std::move(actions_[i]));
    }

    actions_ = std::move(sorted);
  }

  void WriteDirectGraph() {
    std::ofstream out(filename_, std::ios::binary | std::ios::trunc);
    if (!out) {
      ERROR("Cannot write BuildXL fragment: %s", filename_.c_str());
    }
    out << "directgraph 2\n";
    EmitValueTables(out);
    EmitDependencySets(out);
    for (const Action& a : actions_) {
      Emit(out, a);
    }
    if (!out.flush()) {
      ERROR("Cannot write BuildXL fragment: %s", filename_.c_str());
    }
  }

  void EmitValueTables(std::ostream& out) const {
    auto emit = [&](const auto& table, const auto& prefix, size_t bias,
                    bool path) {
      std::vector<std::string_view> values(table.size() + bias);
      std::copy(prefix.begin(), prefix.end(), values.begin());
      for (const auto& p : table) {
        values[p.second] = p.first;
      }
      for (size_t id = 0; id < values.size(); ++id) {
        out << (path ? "path" : "string") << ' ' << id << ' ';
        Literal(out, values[id]);
        out << '\n';
      }
    };
    emit(paths_, config_.paths, path_id_bias_, true);
    emit(strings_, config_.strings, string_id_bias_, false);
  }

  void EmitDependencySets(std::ostream& out) {
    size_t set_id = 0;
    for (const auto& members : config_.sets) {
      out << "set " << set_id++;
      for (size_t path : members) {
        out << " $p" << path;
      }
      out << '\n';
    }
    for (const auto& p : set_counts_) {
      if (p.second < 2) {
        continue;
      }
      set_ids_.emplace(p.first, set_id);
      out << "set " << set_id++;
      for (const auto& path : p.first) {
        out << ' ';
        Value(out, path, true);
      }
      out << '\n';
    }
  }

  void Value(std::ostream& out, std::string_view value, bool path) const {
    if (path) {
      out << "$p" << paths_.at(value);
    } else {
      const auto p = strings_.find(value);
      CHECK(p != strings_.end());
      out << "$s" << p->second;
    }
  }

  void Field(std::ostream& out,
             const char* name,
             std::string_view value,
             bool path = true) const {
    out << "  " << name << ' ';
    Value(out, value, path);
    out << '\n';
  }

  void StringField(std::ostream& out,
                   const char* name,
                   std::string_view value) const {
    out << "  " << name;
    for (std::string_view part : StringParts(value)) {
      out << ' ';
      Value(out, part, false);
    }
    out << '\n';
  }

  void Inputs(std::ostream& out,
              const char* field,
              const std::vector<std::string_view>& list) {
    if (!list.empty() && set_counts_.at(list) > 1) {
      out << "  " << field << " @" << set_ids_.at(list) << '\n';
    } else {
      for (const auto& path : list) {
        Field(out, field, path);
      }
    }
  }

  void InternValues(const Action& a) {
    auto intern = [&a](auto* table, size_t bias, std::string_view value) {
      if (table->find(value) == table->end()) {
        if (!IsValidUtf8(value)) {
          ERROR_LOC(a.node->loc, "BuildXL text requires valid UTF-8");
        }
        table->emplace(value, table->size() + bias);
      }
    };
    auto path = [&](std::string_view value) {
      intern(&paths_, path_id_bias_, value);
    };
    auto str = [&](std::string_view value) {
      intern(&strings_, string_id_bias_, value);
    };
    path(a.shell);
    path(root_);
    for (const auto* list :
         {&a.normal, &a.ordered, &a.outputs, &a.script_inputs}) {
      for (const auto& value : *list) {
        path(value);
      }
    }
    for (const auto& value : a.arguments) {
      for (std::string_view part : StringParts(value)) {
        str(part);
      }
    }
    for (const auto& p : a.environment) {
      str(p.first);
      str(p.second);
    }
    for (const auto& name : config_.passthrough_environment) {
      str(name);
    }
    for (const auto& script : a.scripts) {
      path(script.path);
      for (std::string_view part : StringParts(script.content)) {
        str(part);
      }
    }
  }

  void Emit(std::ostream& out, const Action& a) {
    for (const auto& script : a.scripts) {
      out << "writeFile " << script.name << '\n';
      Field(out, "destination", script.path);
      StringField(out, "content", script.content);
    }
    out << "process " << a.name << '\n';
    Field(out, "executable", a.shell);
    Field(out, "workingDirectory", root_);
    for (const auto& argument : a.arguments) {
      StringField(out, "argument", argument);
    }
    for (const auto& p : a.environment) {
      out << "  environment ";
      Value(out, p.first, false);
      out << ' ';
      Value(out, p.second, false);
      out << '\n';
    }
    for (const auto& name : config_.passthrough_environment) {
      Field(out, "passthroughEnvironment", name, false);
    }
    if (!config_.HasOption("dependsOnCurrentOs") &&
        !config_.HasOption("dependsOnCurrentHostOsDirectories")) {
      out << "  option dependsOnCurrentOs\n";
    }
    for (const auto& row : config_.process) {
      out << "  " << row.tokens[0].value;
      for (size_t i = 1; i < row.tokens.size(); ++i) {
        out << ' ';
        if (row.tokens[i].quoted) {
          Literal(out, row.tokens[i].value);
        } else {
          out << row.tokens[i].value;
        }
      }
      out << '\n';
    }
    Inputs(out, "input", a.normal);
    for (const auto& path : a.script_inputs) {
      Field(out, "input", path);
    }
    Inputs(out, "orderOnly", a.ordered);
    for (const auto& path : a.outputs) {
      Field(out, "output", path);
    }
  }

  // Register a future writeFile producer; script files are not written here.
  std::string_view AddScript(Action& a,
                             size_t index,
                             const std::string& content) {
    const std::string name = a.name + "_script_" + std::to_string(index);
    const std::string_view path =
        InternPath(filename_ + ".scripts/" + name + ".sh");
    if (sources_.count(path)) {
      ERROR_LOC(a.node->loc, "BuildXL script overlaps source input: %.*s",
                SPF(path));
    }
    Claim(path, a.node->loc);
    if (!names_.insert(name).second) {
      ERROR_LOC(a.node->loc, "BuildXL script name collision");
    }
    a.scripts.push_back({name, path, content + "\n"});
    a.script_inputs.push_back(path);
    return path;
  }

  // Preserve recipe shell boundaries and failure handling when combining lines.
  // Spill oversized command text into script producers to stay below argv
  // limits.
  void Lower(Action& a) {
    const size_t limit = 100000;
    std::string shell = Quote(a.shell);
    for (const std::string& argument : a.shell_arguments) {
      shell += " " + Quote(argument);
    }
    a.arguments = a.shell_arguments;
    size_t index = 0;
    for (const Command& c : a.commands) {
      std::string raw;
      TranslateShellCommand(c.cmd.c_str(), &raw, false);
      if (c.cmd.find('\0') != std::string::npos) {
        ERROR_LOC(a.node->loc, "BuildXL NUL-containing recipe");
      }

      if (!a.script.empty()) {
        a.script += " &&\n";
      }

      std::string invocation;
      if (c.force_no_subshell) {
        invocation = "{\n" + raw + "\n}";
      } else if (raw.size() > limit) {
        // This recipe exceeds the inner shell -c argument limit; spilling the
        // outer wrapper wouldn't fix it. Quote the script path as shell text.
        const std::string_view path = AddScript(a, index++, raw);
        invocation = shell + (a.flags == "-ec" ? " -e " : " ") + Quote(path);
      } else {
        invocation = shell + " " + a.flags + " " + Quote(raw);
      }

      if (c.ignore_error) {
        invocation = "(" + invocation + " || true)";
      }

      a.script += invocation;
    }

    if (a.script.size() > limit) {
      // The combined wrapper can exceed the outer shell -c argument limit even
      // if each recipe fits. Its script path is a structured process argument.
      const std::string_view path = AddScript(a, index++, a.script);
      if (a.flags == "-ec") {
        a.arguments.push_back("-e");
      }

      a.arguments.emplace_back(path);
    } else {
      a.arguments.push_back(a.flags);
      a.arguments.push_back(a.script);
    }
  }

  // Owns complete, immutable strings behind all path views. Initialize first
  // and destroy last so root_, filename_, and every other borrower stay valid.
  std::set<std::string, std::less<>> path_pool_;
  Evaluator* ev_;
  CommandEvaluator ce_;
  const std::string& root_;
  const std::string& filename_;
  // Action environments borrow inherited strings here, names from Kati's
  // symbol table, and evaluated override values from the pool below.
  const std::map<std::string, std::string> base_environment_;
  std::set<std::string, std::less<>> environment_values_;
  std::unordered_map<const DepNode*, VisitState> states_;
  std::set<const DepNode*> commandful_;
  std::map<std::string_view, Loc> owners_;
  std::map<std::string_view, Loc> sources_;
  std::set<std::string> names_;
  std::vector<Action> actions_;
  std::map<std::vector<std::string_view>, size_t> set_counts_;
  std::map<std::vector<std::string_view>, size_t> set_ids_;
  std::map<std::string_view, size_t> paths_;
  std::map<std::string, size_t, std::less<>> strings_;
  BuildXLConfig config_;
  size_t path_id_bias_ = 0;
  size_t string_id_bias_ = 0;
};

}  // namespace

void GenerateBuildXL(const std::vector<NamedDepNode>& nodes,
                     Evaluator* ev,
                     const char* filename) {
  BuildXLGenerator(ev, filename).Generate(nodes);
}
