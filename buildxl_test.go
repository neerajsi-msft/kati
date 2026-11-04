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

package kati

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The dg helpers exercise the DirectGraph format emitted by the BuildXL backend.
type dgRef struct {
	SourcePath string
	ProducedBy string
	Path       string
}

type dgOperation struct {
	Name             string
	Kind             string
	Destination      string
	Content          string
	Executable       string
	WorkingDirectory string
	Arguments        []string
	Environment      map[string]string
	Options          []string
	InputFiles       []dgRef
	InputSets        []string
	OrderOnlyInputs  []dgRef
	OrderOnlySets    []string
	OutputFiles      []string
}

type dgFragment struct {
	Root           string
	DependencySets map[string][]dgRef
	Operations     []dgOperation
	Paths          map[string]string
	Strings        map[string]string
}

func dgParse(t *testing.T, data []byte) dgFragment {
	t.Helper()
	g := dgFragment{DependencySets: map[string][]dgRef{}, Paths: map[string]string{}, Strings: map[string]string{}}
	lines := strings.Split(string(data), "\n")
	if lines[0] != "directgraph 2" {
		t.Fatalf("invalid header: %q", lines[0])
	}
	// Keep lexical reference markers distinct from quoted literal '$' and '@'.
	type token struct {
		value string
		bare  bool
	}
	tokens := func(line string) []token {
		var result []token
		for line = strings.TrimSpace(line); line != ""; line = strings.TrimSpace(line) {
			if line[0] != '"' {
				end := strings.IndexAny(line, " \t")
				if end < 0 {
					end = len(line)
				}
				result = append(result, token{line[:end], true})
				line = line[end:]
				continue
			}
			end := 1
			for end < len(line) {
				if line[end] == '\\' {
					end += 2
				} else if line[end] == '"' {
					break
				} else {
					end++
				}
			}
			if end >= len(line) || (end+1 < len(line) && line[end+1] != ' ' && line[end+1] != '\t') {
				t.Fatalf("invalid quoted token: %q", line)
			}
			value, err := strconv.Unquote(line[:end+1])
			if err != nil {
				t.Fatalf("invalid literal %q: %v", line, err)
			}
			result = append(result, token{value, false})
			line = line[end+1:]
		}
		return result
	}
	resolve := func(tok token) string {
		if tok.bare && strings.HasPrefix(tok.value, "$") {
			table := g.Paths
			if strings.HasPrefix(tok.value, "$s") {
				table = g.Strings
			} else if !strings.HasPrefix(tok.value, "$p") {
				t.Fatalf("invalid intern reference %s", tok.value)
			}
			value, exists := table[tok.value[2:]]
			if !exists {
				t.Fatalf("undefined intern reference %s", tok.value)
			}
			return value
		}
		return tok.value
	}
	var op *dgOperation
	for number, line := range lines[1:] {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		fields := tokens(line)
		if !strings.HasPrefix(line, " ") {
			op = nil
			if len(fields) < 2 {
				t.Fatalf("invalid record at line %d: %s", number+2, line)
			}
			name := fields[1].value
			switch fields[0].value {
			case "path", "string":
				table := g.Paths
				if fields[0].value == "string" {
					table = g.Strings
				}
				if name != strconv.Itoa(len(table)) || len(fields) != 3 || fields[2].bare {
					t.Fatalf("invalid dense registration: %s", line)
				}
				table[name] = fields[2].value
			case "set":
				if name != strconv.Itoa(len(g.DependencySets)) {
					t.Fatalf("invalid dense set: %s", line)
				}
				g.DependencySets[name] = []dgRef{}
				for _, field := range fields[2:] {
					if !field.bare || !strings.HasPrefix(field.value, "$p") {
						t.Fatalf("set requires path IDs: %s", line)
					}
					g.DependencySets[name] = append(g.DependencySets[name], dgRef{Path: resolve(field)})
				}
			case "process", "writeFile":
				if len(fields) != 2 {
					t.Fatalf("invalid operation: %s", line)
				}
				g.Operations = append(g.Operations, dgOperation{
					Name: name, Kind: fields[0].value, Environment: map[string]string{},
				})
				op = &g.Operations[len(g.Operations)-1]
			default:
				t.Fatalf("unexpected Kati record: %s", line)
			}
			continue
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") || op == nil || len(fields) < 2 {
			t.Fatalf("invalid field at line %d: %s", number+2, line)
		}
		field, value := fields[0].value, resolve(fields[1])
		if field != "option" && !(fields[1].bare && strings.HasPrefix(fields[1].value, "$")) &&
			!((field == "input" || field == "orderOnly") && fields[1].bare && strings.HasPrefix(fields[1].value, "@")) {
			t.Fatalf("field requires interned value: %s", line)
		}
		if field == "environment" {
			if len(fields) != 3 {
				t.Fatalf("invalid environment: %s", line)
			}
			op.Environment[value] = resolve(fields[2])
			continue
		}
		if field == "argument" || field == "content" {
			var argument strings.Builder
			for _, part := range fields[1:] {
				if !part.bare || !strings.HasPrefix(part.value, "$s") {
					t.Fatalf("%s requires string IDs: %s", field, line)
				}
				argument.WriteString(resolve(part))
			}
			if field == "argument" {
				op.Arguments = append(op.Arguments, argument.String())
			} else {
				op.Content = argument.String()
			}
			continue
		}
		if len(fields) != 2 {
			t.Fatalf("invalid field arity: %s", line)
		}
		switch field {
		case "executable":
			op.Executable = value
		case "workingDirectory":
			op.WorkingDirectory = value
			g.Root = value
		case "option":
			op.Options = append(op.Options, value)
		case "destination":
			op.Destination = value
		case "output":
			op.OutputFiles = append(op.OutputFiles, value)
		case "input", "orderOnly":
			refs, sets := &op.InputFiles, &op.InputSets
			if field == "orderOnly" {
				refs, sets = &op.OrderOnlyInputs, &op.OrderOnlySets
			}
			if fields[1].bare && strings.HasPrefix(fields[1].value, "@") {
				name := strings.TrimPrefix(fields[1].value, "@")
				if _, exists := g.DependencySets[name]; !exists {
					t.Fatalf("undefined dependency set: %s", line)
				}
				*sets = append(*sets, name)
			} else {
				*refs = append(*refs, dgRef{Path: value})
			}
		default:
			t.Fatalf("unknown field: %s", line)
		}
	}
	// Classify paths only in the harness; there is no ownership syntax on wire.
	owners := map[string]string{}
	for _, op := range g.Operations {
		for _, path := range op.OutputFiles {
			owners[path] = op.Name
		}
		if op.Kind == "writeFile" {
			owners[op.Destination] = op.Name
		}
	}
	classify := func(refs []dgRef) {
		for i := range refs {
			if owner := owners[refs[i].Path]; owner != "" {
				refs[i].ProducedBy = owner
			} else {
				refs[i].SourcePath, refs[i].Path = refs[i].Path, ""
			}
		}
	}
	for _, set := range g.DependencySets {
		classify(set)
	}
	for i := range g.Operations {
		classify(g.Operations[i].InputFiles)
		classify(g.Operations[i].OrderOnlyInputs)
	}
	return g
}

func dgDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "kati-buildxl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func dgWrite(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func dgGenerate(t *testing.T, dir string, args ...string) (dgFragment, []byte) {
	t.Helper()
	binary, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, append([]string{"--buildxl=fragment.dg"}, args...)...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generation: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fragment.dg"))
	if err != nil {
		t.Fatal(err)
	}
	graph := dgParse(t, data)
	if len(graph.Operations) > 0 && graph.Root != dir {
		t.Fatalf("invalid fragment header: %+v", graph)
	}
	return graph, data
}

func (g dgFragment) inputs(t *testing.T, op dgOperation, orderOnly bool) []dgRef {
	t.Helper()
	refs, sets := op.InputFiles, op.InputSets
	if orderOnly {
		refs, sets = op.OrderOnlyInputs, op.OrderOnlySets
	}
	result := append([]dgRef(nil), refs...)
	for _, name := range sets {
		set, exists := g.DependencySets[name]
		if !exists {
			t.Fatalf("undefined dependency set %q", name)
		}
		result = append(result, set...)
	}
	return result
}

// This harness verifies the textual contract, then runs each declared action
// once in postorder. It is not a cache or a replacement for BuildXL integration.
func dgExecute(t *testing.T, g dgFragment, logs ...*strings.Builder) error {
	t.Helper()
	produced := map[string]string{}
	names := map[string]bool{}
	check := func(ref dgRef) error {
		if ref.SourcePath != "" {
			if ref.ProducedBy != "" || ref.Path != "" {
				return fmt.Errorf("ambiguous reference %+v", ref)
			}
			_, err := os.Stat(ref.SourcePath)
			return err
		}
		if ref.ProducedBy == "" || produced[ref.Path] != ref.ProducedBy {
			return fmt.Errorf("missing or forward producer %+v", ref)
		}
		_, err := os.Stat(ref.Path)
		return err
	}
	for _, op := range g.Operations {
		if op.Name == "" || names[op.Name] {
			return fmt.Errorf("duplicate operation %q", op.Name)
		}
		names[op.Name] = true
		for _, ref := range append(g.inputs(t, op, false), g.inputs(t, op, true)...) {
			if err := check(ref); err != nil {
				return err
			}
		}
		outputs := op.OutputFiles
		switch op.Kind {
		case "writeFile":
			if err := os.MkdirAll(filepath.Dir(op.Destination), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(op.Destination, []byte(op.Content), 0644); err != nil {
				return err
			}
			outputs = []string{op.Destination}
		case "process":
			if !reflect.DeepEqual(op.Options, []string{"dependsOnCurrentOs"}) {
				return fmt.Errorf("invalid options %v", op.Options)
			}
			cmd := exec.Command(op.Executable, op.Arguments...)
			cmd.Dir = op.WorkingDirectory
			keys := make([]string, 0, len(op.Environment))
			for key := range op.Environment {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			cmd.Env = []string{}
			for _, key := range keys {
				cmd.Env = append(cmd.Env, key+"="+op.Environment[key])
			}
			output, err := cmd.CombinedOutput()
			for _, log := range logs {
				log.Write(output)
			}
			if err != nil {
				return fmt.Errorf("action %s: %v\n%s", op.Name, err, output)
			}
		default:
			return fmt.Errorf("unknown operation kind %q", op.Kind)
		}
		for _, output := range outputs {
			if produced[output] != "" {
				return fmt.Errorf("duplicate output %s", output)
			}
			info, err := os.Stat(output)
			if err != nil {
				return fmt.Errorf("missing declared output %s: %w", output, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("not a file output %s", output)
			}
			produced[output] = op.Name
		}
	}
	return nil
}

func dgProcess(t *testing.T, g dgFragment, output string) dgOperation {
	t.Helper()
	for _, op := range g.Operations {
		for _, path := range op.OutputFiles {
			if path == filepath.Join(g.Root, output) {
				return op
			}
		}
	}
	t.Fatalf("no producer for %s", output)
	return dgOperation{}
}

func TestBuildXLCommandLineOptions(t *testing.T) {
	binary, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		args       []string
		diagnostic string
	}{
		{"separate-output", []string{"--buildxl", "fragment.dg"}, ""},
		{"empty-output", []string{"--buildxl="}, "requires a nonempty output filename"},
		{"removed-option", []string{"--direct_graph=fragment.dg"}, "Unknown flag"},
		{"removed-option-separate-output", []string{"--direct_graph", "fragment.dg"}, "Unknown flag"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dir := dgDir(t)
			dgWrite(t, dir, "Makefile", "out: ; @touch out\n")
			cmd := exec.Command(binary, test.args...)
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			fragment := filepath.Join(dir, "fragment.dg")
			if test.diagnostic != "" {
				if err == nil || !strings.Contains(string(output), test.diagnostic) {
					t.Fatalf("expected %q: %v\n%s", test.diagnostic, err, output)
				}
				if _, err := os.Stat(fragment); !os.IsNotExist(err) {
					t.Fatal("invalid options should not publish a fragment")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildXL export failed: %v\n%s", err, output)
			}
			data, err := os.ReadFile(fragment)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(data), "directgraph 2\n") {
				t.Fatal("expected unchanged DirectGraph header")
			}
			if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
				t.Fatal("BuildXL export executed a recipe")
			}
		})
	}
}

func TestBuildXLActions(t *testing.T) {
	dir := dgDir(t)
	t.Setenv("DG_UNEXPORTED", "must disappear")
	t.Setenv("DG_INHERITED", "base=preserved")
	dgWrite(t, dir, "input", "source\n")
	dgWrite(t, dir, "ordered", "ordered\n")
	dgWrite(t, dir, "Makefile", `.PHONY: all alias nested orderalias
export DG_SCOPE = global
unexport DG_UNEXPORTED
all: consumer other
alias: nested | orderalias
nested: second input input
orderalias: ordered
first second &: input
	@cat $< > first
	@cp first second
consumer: DG_SCOPE = scoped
consumer: alias
	@private=leak
	@printf '%s\n' "$$DG_SCOPE" "$${private-unset}" "$${DG_UNEXPORTED-unset}" '$$literal' '$@' '$<' > consumer
	@-false
	@cat second >> consumer
other: alias
	@cat second > other
`)
	g, data := dgGenerate(t, dir)
	if _, err := os.Stat(filepath.Join(dir, "first")); !os.IsNotExist(err) {
		t.Fatal("emission must not execute recipes")
	}
	first, second := dgProcess(t, g, "first"), dgProcess(t, g, "second")
	if first.Name != second.Name || len(first.OutputFiles) != 2 {
		t.Fatal("grouped outputs must share one producer")
	}
	consumer := dgProcess(t, g, "consumer")
	if consumer.Environment["DG_SCOPE"] != "scoped" {
		t.Fatalf("target export scope lost: %+v", consumer.Environment)
	}
	other := dgProcess(t, g, "other")
	if other.Environment["DG_SCOPE"] != "global" {
		t.Fatal("target-scoped export leaked into another action")
	}
	for _, action := range []dgOperation{first, consumer, other} {
		if action.Environment["DG_INHERITED"] != "base=preserved" {
			t.Fatal("inherited environment value changed between actions")
		}
		if _, exists := action.Environment["DG_UNEXPORTED"]; exists {
			t.Fatal("unexported environment value survived")
		}
	}
	normal, ordered := g.inputs(t, consumer, false), g.inputs(t, consumer, true)
	wantNormal := []dgRef{
		{ProducedBy: first.Name, Path: filepath.Join(dir, "second")},
		{SourcePath: filepath.Join(dir, "input")},
		{SourcePath: filepath.Join(dir, "input")},
	}
	if !reflect.DeepEqual(normal, wantNormal) ||
		!reflect.DeepEqual(ordered, []dgRef{{SourcePath: filepath.Join(dir, "ordered")}}) {
		t.Fatalf("flattening lost order/multiplicity: normal=%+v ordered=%+v", normal, ordered)
	}
	if len(consumer.InputSets) != 1 || len(consumer.OrderOnlySets) != 1 {
		t.Fatal("repeated exact lists should be shared")
	}
	_, again := dgGenerate(t, dir)
	if string(data) != string(again) {
		t.Fatal("emission should be deterministic in a fixed environment")
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(dir, "consumer"))
	if err != nil || string(text) != "scoped\nunset\nunset\n$literal\nconsumer\nalias\nsource\n" {
		t.Fatalf("literal/automatic/scope/shell semantics: %v\n%s", err, text)
	}
}

func TestBuildXLTextEscapingAndFactoring(t *testing.T) {
	dir := filepath.Join(dgDir(t), "quoted\" backslash\\ dollar$ unicode-é")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	literal := "$HOME @not-a-set \"quote\" \\slash\nline\rtab\t\x01\x7f\u0080\u0085\u009f é\u00a0\u07ff\u0800\uffff\U0001f600\U0010ffff " + strings.Repeat("repeat ", 256)
	t.Setenv("DG_LITERAL", literal)
	t.Setenv("DG_EMPTY", "")
	dgWrite(t, dir, "input", "source\n")
	dgWrite(t, dir, "ordered", "ordered\n")
	dgWrite(t, dir, "Makefile", "all: one two\none two: input input | ordered\n\t@printf '%s' \"$$DG_LITERAL\" > $@\n")
	g, data := dgGenerate(t, dir)
	atom := ""
	for name, value := range g.Strings {
		if value == literal {
			atom = name
		}
	}
	if atom == "" || strings.Count(string(data), "$s"+atom+"\n") != 2 {
		t.Fatal("repeated environment literals must use one string ID and two references")
	}
	for _, escaped := range []string{`\"quote\"`, `\\slash`, `\nline\rtab\t\u0001\u007f\u0080\u0085\u009f`} {
		if !strings.Contains(string(data), escaped) {
			t.Fatalf("missing text escape %q", escaped)
		}
	}
	for _, output := range []string{"one", "two"} {
		op := dgProcess(t, g, output)
		empty, exists := op.Environment["DG_EMPTY"]
		if op.Environment["DG_LITERAL"] != literal || !exists || empty != "" ||
			len(op.InputSets) != 1 || len(op.OrderOnlySets) != 1 {
			t.Fatal("literal environment or repeated dependency factoring changed")
		}
		refs := g.inputs(t, op, false)
		if len(refs) != 2 || refs[0] != refs[1] {
			t.Fatal("dependency sets must preserve duplicates")
		}
	}
	_, again := dgGenerate(t, dir)
	if string(again) != string(data) {
		t.Fatal("intern/set IDs or output ordering are nondeterministic")
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"one", "two"} {
		actual, err := os.ReadFile(filepath.Join(dir, output))
		if err != nil || string(actual) != literal {
			t.Fatalf("escaped value was expanded or changed: %v\n%q", err, actual)
		}
	}
}

func TestBuildXLArgumentsShareLosslessWhitespaceAndQuoteChunks(t *testing.T) {
	dir := dgDir(t)
	dgWrite(t, dir, "Makefile", "all: one two\none two:\n\t@printf '%s' 'same  words\tand \"quotes\" $$literal \\\\tail `not-a-command`' > $@\n")
	g, data := dgGenerate(t, dir)
	var shared string
	for id, value := range g.Strings {
		if value == "words" {
			shared = "$s" + id
		}
	}
	if shared == "" || strings.Count(string(data), shared+" ") < 2 {
		t.Fatal("recipe words should be interned once and referenced by both actions")
	}
	for _, chunk := range []string{"\"", "'", "`", "quotes", "not-a-command"} {
		found := false
		for _, value := range g.Strings {
			found = found || value == chunk
		}
		if !found {
			t.Fatalf("quote boundary did not expose chunk %q", chunk)
		}
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != "same  words\tand \"quotes\" $literal \\\\tail `not-a-command`" {
			t.Fatalf("factoring changed shell bytes: %q, %v", got, err)
		}
	}
}

func TestBuildXLOrderOnlyFlattening(t *testing.T) {
	dir := dgDir(t)
	dgWrite(t, dir, "input", "source")
	dgWrite(t, dir, "Makefile", `.PHONY: alias nested
out: | alias
	@printf '%s' ordered > out
alias: nested | input
nested: built
built: input
	@cp input built
`)
	g, _ := dgGenerate(t, dir)
	out, built := dgProcess(t, g, "out"), dgProcess(t, g, "built")
	if len(g.inputs(t, out, false)) != 0 ||
		!reflect.DeepEqual(g.inputs(t, out, true), []dgRef{
			{ProducedBy: built.Name, Path: filepath.Join(dir, "built")},
			{SourcePath: filepath.Join(dir, "input")},
		}) {
		t.Fatal("order-only alias must promote all nested dependencies to order-only")
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
}

func TestBuildXLLongCommands(t *testing.T) {
	dir := dgDir(t)
	long := strings.Repeat("x", 160000)
	dgWrite(t, dir, "input", "source")
	dgWrite(t, dir, "ordered", "ordered")
	dgWrite(t, dir, "Makefile", "all: out other\nout: input | ordered\n"+
		"\t@printf '%s' '"+long+"' > out\n\t@printf '%s' '$$literal' >> out\n"+
		"other: input | ordered\n\t@touch other\n")
	g, fragment := dgGenerate(t, dir)
	op := dgProcess(t, g, "out")
	if len(g.Operations) < 2 || g.Operations[0].Kind != "writeFile" {
		t.Fatal("long recipes must lower to declared writeFile scripts")
	}
	factored := false
	for _, line := range strings.Split(string(fragment), "\n") {
		factored = factored || strings.HasPrefix(line, "  content ") && len(strings.Fields(line)) > 2
	}
	if !factored {
		t.Fatal("generated script content should concatenate interned chunks")
	}
	if len(g.inputs(t, op, false)) != 2 || len(op.InputSets) != 1 || len(op.OrderOnlySets) != 1 {
		t.Fatal("script input must remain explicit alongside shared dependency sets")
	}
	for _, arg := range op.Arguments {
		if len(arg) > 100000 {
			t.Fatal("lowered command still exceeds argv limit")
		}
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil || string(data) != long+"$literal" {
		t.Fatal("long command literals changed")
	}
}

func TestBuildXLCommandlessRulesMatchNinjaFlattening(t *testing.T) {
	for _, phony := range []bool{false, true} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("phony=%t/existing=%t", phony, existing), func(t *testing.T) {
				dir := dgDir(t)
				dgWrite(t, dir, "input", "source\n")
				if existing {
					dgWrite(t, dir, "empty", "not a source input\n")
				}
				prefix := ""
				if phony {
					prefix = ".PHONY: alias nested empty orderedAlias\n"
				}
				dgWrite(t, dir, "Makefile", prefix+`out: alias empty | orderedAlias
	@cp built out
alias: nested input | ordered
	@$(EMPTY)
nested: built
empty: ; @$(EMPTY)
orderedAlias: ordered
built: input
	@cp input built
ordered:
	@touch ordered
unrelated:
	@touch unrelated
`)
				g, _ := dgGenerate(t, dir, "out")
				out, built, ordered := dgProcess(t, g, "out"), dgProcess(t, g, "built"), dgProcess(t, g, "ordered")
				if len(g.Operations) != 3 ||
					!reflect.DeepEqual(g.inputs(t, out, false), []dgRef{
						{ProducedBy: built.Name, Path: filepath.Join(dir, "built")},
						{SourcePath: filepath.Join(dir, "input")},
					}) ||
					!reflect.DeepEqual(g.inputs(t, out, true), []dgRef{
						{ProducedBy: ordered.Name, Path: filepath.Join(dir, "ordered")},
						{ProducedBy: ordered.Name, Path: filepath.Join(dir, "ordered")},
					}) {
					t.Fatal("commandless rules must flatten to their normal/order-only prerequisites without stamp inputs or unrelated actions")
				}
				if err := dgExecute(t, g); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{"alias", "nested", "orderedAlias", "unrelated"} {
					if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
						t.Fatalf("structural or unselected rule created %s: %v", path, err)
					}
				}
				if data, err := os.ReadFile(filepath.Join(dir, "empty")); existing {
					if err != nil || string(data) != "not a source input\n" {
						t.Fatal("existing commandless output was changed")
					}
				} else if !os.IsNotExist(err) {
					t.Fatal("commandless rule created its missing output")
				}
			})
		}
	}
}

func TestBuildXLCommandlessRoots(t *testing.T) {
	for _, rule := range []string{"empty:\n", "empty: ;\n", "empty: ; @$(EMPTY)\n", ".PHONY: empty\nempty:\n"} {
		dir := dgDir(t)
		dgWrite(t, dir, "Makefile", rule)
		g, data := dgGenerate(t, dir, "empty")
		if len(g.Operations) != 0 || string(data) != "directgraph 2\n" {
			t.Fatal("an input-free commandless root must contribute no pip or file")
		}
	}
}

func TestBuildXLMissingGroupedOutput(t *testing.T) {
	dir := dgDir(t)
	dgWrite(t, dir, "Makefile", "first second &: ; @touch first\n")
	g, _ := dgGenerate(t, dir)
	if err := dgExecute(t, g); err == nil || !strings.Contains(err.Error(), "missing declared output") {
		t.Fatalf("missing grouped output must not be accepted: %v", err)
	}
}

func TestBuildXLGeneratedHeaderCompile(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler required for generated-header compile coverage")
	}
	compiler, err = filepath.Abs(compiler)
	if err != nil {
		t.Fatal(err)
	}
	dir := dgDir(t)
	dgWrite(t, dir, "consumer.c", "#include \"generated.h\"\nint generated_value(void) { return GENERATED_VALUE; }\n")
	dgWrite(t, dir, "Makefile",
		"consumer.o: consumer.c generated.h\n\t@"+compiler+" -nostdinc -ffreestanding -c consumer.c -o consumer.o\n"+
			"generated.h:\n\t@printf '%s\\n' '#define GENERATED_VALUE 37' > generated.h\n")
	g, _ := dgGenerate(t, dir)
	compile := dgProcess(t, g, "consumer.o")
	header := dgProcess(t, g, "generated.h")
	if !reflect.DeepEqual(g.inputs(t, compile, false), []dgRef{
		{SourcePath: filepath.Join(dir, "consumer.c")},
		{ProducedBy: header.Name, Path: filepath.Join(dir, "generated.h")},
	}) || len(g.inputs(t, compile, true)) != 0 {
		t.Fatal("C compilation must declare generated header as a normal producer input")
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
	object, err := os.ReadFile(filepath.Join(dir, "consumer.o"))
	if err != nil || len(object) < 4 || string(object[:4]) != "\x7fELF" {
		t.Fatalf("generated-header compilation did not produce an ELF object: %v", err)
	}
}

func TestBuildXLFixtureGenerator(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for fixture generator")
	}
	script, err := filepath.Abs(filepath.Join("examples", "buildxl", "generate_fixture.py"))
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dgDir(t), "fixture")
	run := func() ([]byte, error) {
		return exec.Command(python, script, destination).CombinedOutput()
	}
	if output, err := run(); err != nil {
		t.Fatalf("fixture generation: %v\n%s", err, output)
	}
	for _, output := range []string{
		"producer/generated.h", "producer/generated-input.txt", "consumer/consumer.out",
	} {
		if _, err := os.Stat(filepath.Join(destination, filepath.FromSlash(output))); !os.IsNotExist(err) {
			t.Fatalf("generator executed a recipe or produced %s: %v", output, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(destination, "projects.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "root consumer\n"+
		"project tools tools.dg\n"+
		"project producer producer.dg depends tools\n"+
		"project consumer consumer.dg depends tools producer\n"+
		"export producer file producer/generated.h\n"+
		"export producer file producer/generated-input.txt\n"+
		"export consumer file consumer/consumer.out\n" {
		t.Fatalf("invalid fixture project metadata: %s", data)
	}
	tools, err := os.ReadFile(filepath.Join(destination, "tools.dg"))
	if err != nil {
		t.Fatal(err)
	}
	for id, root := range []string{"/usr", "/bin"} {
		if !strings.Contains(string(tools), fmt.Sprintf("path %d %s\n", id, strconv.Quote(root))) ||
			!strings.Contains(string(tools), fmt.Sprintf("  directoryRoot $p%d\n", id)) ||
			!strings.Contains(string(tools), fmt.Sprintf("  directory $p%d\n", id)) {
			t.Fatalf("missing source seal/export for %s: %s", root, tools)
		}
	}
	if strings.Contains(string(tools), destination) ||
		strings.Count(string(tools), "  sealKind sourceAllDirectories\n") != 2 {
		t.Fatal("tools must seal host directories only, not generated outputs")
	}
	for _, project := range []string{"producer", "consumer"} {
		fragment, err := os.ReadFile(filepath.Join(destination, project+".dg"))
		if err != nil {
			t.Fatal(err)
		}
		graph := dgParse(t, fragment)
		if graph.Root != filepath.Join(destination, project) {
			t.Fatalf("incorrect evaluation root: %s", graph.Root)
		}
		for _, operation := range graph.Operations {
			if !reflect.DeepEqual(operation.Environment, map[string]string{
				"PATH": "/usr/bin:/bin", "HOME": destination,
				"TMPDIR": filepath.Join(destination, "compiler-scratch"), "LC_ALL": "C",
			}) {
				t.Fatalf("fixture environment is not minimal: %+v", operation.Environment)
			}
		}
		if err := dgExecute(t, graph); err != nil {
			t.Fatal(err)
		}
	}
	expected := "#define GENERATED_VALUE 37\ndirectgraph-input\n"
	resultPath := filepath.Join(destination, "consumer", "consumer.out")
	if result, err := os.ReadFile(resultPath); err != nil || string(result) != expected {
		t.Fatalf("fixture result: %v\n%s", err, result)
	}
	if output, err := run(); err == nil || !strings.Contains(string(output), "already exists") {
		t.Fatalf("generator must refuse an existing destination: %v\n%s", err, output)
	}
	if result, err := os.ReadFile(resultPath); err != nil || string(result) != expected {
		t.Fatal("existing output was deleted or changed")
	}
}

func TestBuildXLNormalizedProducerOrder(t *testing.T) {
	dir := dgDir(t)
	dgWrite(t, dir, "input", "source\n")
	dgWrite(t, dir, "Makefile", "all: consumer first\nconsumer: "+dir+"/first\n\t@cp $< consumer\nfirst: input\n\t@cp input first\n")
	g, _ := dgGenerate(t, dir)
	consumer, first := dgProcess(t, g, "consumer"), dgProcess(t, g, "first")
	if !reflect.DeepEqual(g.inputs(t, consumer, false),
		[]dgRef{{ProducedBy: first.Name, Path: filepath.Join(dir, "first")}}) {
		t.Fatal("absolute prerequisite must resolve to local relative producer")
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
}

func TestBuildXLLongAggregateScript(t *testing.T) {
	dir := dgDir(t)
	line := "\t@printf '%s' '" + strings.Repeat("x", 40000) + "' >> out\n"
	dgWrite(t, dir, "Makefile", "out:\n"+strings.Repeat(line, 4))
	g, _ := dgGenerate(t, dir)
	op := dgProcess(t, g, "out")
	if len(op.Arguments) != 1 || len(g.inputs(t, op, false)) != 1 {
		t.Fatal("combined short recipes exceeding argv limit need a declared aggregate script")
	}
	if err := dgExecute(t, g); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "out"))
	if string(data) != strings.Repeat("x", 160000) {
		t.Fatal("aggregate script changed recipe semantics")
	}
}

func TestBuildXLFailureStopsRecipe(t *testing.T) {
	for _, posix := range []bool{false, true} {
		t.Run(fmt.Sprint(posix), func(t *testing.T) {
			dir := dgDir(t)
			prefix := ""
			recipe := "\t@false\n\t@touch out\n"
			if posix {
				prefix = ".POSIX:\n"
				recipe = "\t@false; touch out\n"
			}
			dgWrite(t, dir, "Makefile", prefix+"out:\n"+recipe)
			g, _ := dgGenerate(t, dir)
			if err := dgExecute(t, g); err == nil || !strings.Contains(err.Error(), "action ") {
				t.Fatalf("failed shell must fail action: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
				t.Fatal("failed shell continued to subsequent command")
			}
		})
	}
}

func TestBuildXLShellExecutableAndOptions(t *testing.T) {
	for _, mode := range []string{"inline", "inner-script", "aggregate-script"} {
		for _, posix := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/posix=%v", mode, posix), func(t *testing.T) {
				dir := dgDir(t)
				shell := filepath.Join(dir, "bash shell")
				if err := os.Symlink("/bin/bash", shell); err != nil {
					t.Fatal(err)
				}
				shellValue := "'" + shell + "' -o \"pipefail\""
				if mode == "inline" {
					shell = "/bin/bash"
					shellValue = shell + " -o pipefail"
				} else if mode == "aggregate-script" {
					shellValue = strings.ReplaceAll(shell, " ", "\\ ") + " -o pipefail"
				}
				prefix := "SHELL = " + shellValue + "\n"
				if posix {
					prefix += ".POSIX:\n"
				}
				padding := ""
				switch mode {
				case "inner-script":
					padding = ": " + strings.Repeat("x", 110000) + "; "
				case "aggregate-script":
					padding = strings.Repeat("\t@: "+strings.Repeat("x", 40000)+"\n", 4)
				}
				recipe := "\t@" + padding + "printf '%s' \"$$SHELLOPTS\" > out\n"
				failure := "\t@" + padding + "false | true\n\t@touch out\n"
				if mode == "aggregate-script" {
					recipe = padding + "\t@printf '%s' \"$$SHELLOPTS\" > out\n"
					failure = padding + "\t@false | true\n\t@touch out\n"
				}
				dgWrite(t, dir, "Makefile", prefix+"out:\n"+recipe)
				g, _ := dgGenerate(t, dir)
				op := dgProcess(t, g, "out")
				if op.Executable != shell || len(op.Arguments) < 3 ||
					!reflect.DeepEqual(op.Arguments[:2], []string{"-o", "pipefail"}) {
					t.Fatalf("shell executable/options not separated: %q %v", op.Executable, op.Arguments)
				}
				if err := dgExecute(t, g); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "out"))
				if err != nil || !strings.Contains(string(data), "pipefail") {
					t.Fatalf("recipe shell lost options: %q, %v", data, err)
				}
				if err := os.Remove(filepath.Join(dir, "out")); err != nil {
					t.Fatal(err)
				}
				dgWrite(t, dir, "Makefile", prefix+"out:\n"+failure)
				g, _ = dgGenerate(t, dir)
				if err := dgExecute(t, g); err == nil || !strings.Contains(err.Error(), "action ") {
					t.Fatalf("pipefail must fail the action: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
					t.Fatal("failed pipeline continued to subsequent recipe")
				}
			})
		}
	}
}

func TestBuildXLDelayedEffectsAndPosix(t *testing.T) {
	for _, posix := range []bool{false, true} {
		t.Run(fmt.Sprint(posix), func(t *testing.T) {
			dir := dgDir(t)
			prefix := ""
			if posix {
				prefix = ".POSIX:\n"
			}
			dgWrite(t, dir, "Makefile", prefix+"out:\n\t@$(info runtime-info $$literal 'quote')printf '%s\\n' '$$quoted' > out # comment\n\t@-false\n\t@printf '%s\\n' done >> out\n")
			g, _ := dgGenerate(t, dir)
			var log strings.Builder
			if err := dgExecute(t, g, &log); err != nil {
				t.Fatal(err)
			}
			if log.String() != "runtime-info $literal 'quote'\n" {
				t.Fatalf("delayed diagnostic literal changed: %s", log.String())
			}
			data, _ := os.ReadFile(filepath.Join(dir, "out"))
			if string(data) != "$quoted\ndone\n" {
				t.Fatalf("raw recipe/delayed effects changed: %s", data)
			}
		})
	}
}

func TestBuildXLDelayedError(t *testing.T) {
	dir := dgDir(t)
	dgWrite(t, dir, "Makefile", "out:\n\t@$(warning warning $$literal 'quote')$(error failure $$literal 'quote')touch out\n")
	g, _ := dgGenerate(t, dir)
	var log strings.Builder
	if err := dgExecute(t, g, &log); err == nil {
		t.Fatal("delayed error must fail execution, not graph generation")
	}
	if !strings.Contains(log.String(), "warning $literal 'quote'") ||
		!strings.Contains(log.String(), "*** failure $literal 'quote'.") {
		t.Fatalf("delayed diagnostics lost literals: %s", log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatal("recipe ran after delayed error")
	}
}

func TestBuildXLRejectSymlinkArtifact(t *testing.T) {
	dir := dgDir(t)
	dgWrite(t, dir, "actual", "source")
	if err := os.Symlink("actual", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	dgWrite(t, dir, "Makefile", "out: link\n\t@cp link out\n")
	binary, _ := filepath.Abs("ckati")
	cmd := exec.Command(binary, "--buildxl=fragment.dg")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "symlink artifact paths") {
		t.Fatalf("symlink aliases must be explicitly rejected: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Makefile:2:") {
		t.Fatalf("expected the consuming action's location: %s", output)
	}
}

func TestBuildXLActionErrorLocations(t *testing.T) {
	cases := map[string]struct {
		makefile   string
		diagnostic string
		location   string
	}{
		"fragment-output": {
			".PHONY: all\nall: fragment.dg\nfragment.dg: ; @touch $@\n",
			"filename overlaps",
			"Makefile:3:",
		},
		"fragment-input": {
			".PHONY: all\nall: out\nout: fragment.dg\n\t@touch out\n",
			"filename overlaps",
			"Makefile:4:",
		},
		"duplicate-owner": {
			".PHONY: all\nall: out @ROOT@/out\nout @ROOT@/out: ; @touch $@\n",
			"duplicate output ownership",
			"Makefile:3:",
		},
	}
	binary, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			dir := dgDir(t)
			dgWrite(t, dir, "Makefile", strings.ReplaceAll(test.makefile, "@ROOT@", dir))
			cmd := exec.Command(binary, "--buildxl=fragment.dg")
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.diagnostic) ||
				!strings.Contains(string(output), test.location) {
				t.Fatalf("expected action location and %q: %v\n%s", test.diagnostic, err, output)
			}
			if _, err := os.Stat(filepath.Join(dir, "fragment.dg")); !os.IsNotExist(err) {
				t.Fatal("failed generation should not publish a fragment")
			}
		})
	}
}

func TestBuildXLSpecialTargetsMatchNinja(t *testing.T) {
	for _, declaration := range []string{
		".SECONDARY: generated.h\n", ".SECONDARY:\n",
		".PRECIOUS: generated.h\n", ".PRECIOUS:\n",
		".INTERMEDIATE: generated.h\n", ".INTERMEDIATE:\n",
		".DELETE_ON_ERROR:\n",
		".ONESHELL:\n",
		".SILENT: generated.h\n", ".SILENT:\n",
		".IGNORE: generated.h\n", ".IGNORE:\n",
		".NOTPARALLEL:\n",
		".EXPORT_ALL_VARIABLES:\n",
		".LOW_RESOLUTION_TIME: generated.h\n", ".LOW_RESOLUTION_TIME:\n",
	} {
		t.Run(strings.TrimSpace(declaration), func(t *testing.T) {
			dir := dgDir(t)
			makefile := "out: generated.h\n\t@cat generated.h > out\n" +
				"generated.h:\n\t@printf 'generated\\n' > generated.h\n"
			dgWrite(t, dir, "Makefile", makefile)
			_, baseline := dgGenerate(t, dir)
			dgWrite(t, dir, "Makefile", makefile+declaration)
			graph, data := dgGenerate(t, dir)
			if string(data) != string(baseline) {
				t.Fatal("ignored special targets must not alter the exported graph, matching Kati-to-Ninja semantics")
			}
			if err := dgExecute(t, graph); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"generated.h", "out"} {
				content, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(content) != "generated\n" {
					t.Fatalf("%s: content %q, error %v", name, content, err)
				}
			}
		})
	}
}

func TestBuildXLRejectInvalidUtf8BeforePublishing(t *testing.T) {
	binary, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"continuation":     "\x80",
		"overlong-two":     "\xc0\x80",
		"overlong-three":   "\xe0\x80\x80",
		"overlong-four":    "\xf0\x80\x80\x80",
		"truncated":        "\xe2\x82",
		"surrogate":        "\xed\xa0\x80",
		"out-of-range":     "\xf4\x90\x80\x80",
		"invalid-leading":  "\xf5\x80\x80\x80",
		"invalid-trailing": "\xe2\x28\xa1",
	} {
		t.Run(name, func(t *testing.T) {
			dir := dgDir(t)
			dgWrite(t, dir, "Makefile", "out: ; @touch out\n")
			dgWrite(t, dir, "fragment.dg", "previous fragment")
			cmd := exec.Command(binary, "--buildxl=fragment.dg")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "DG_INVALID_UTF8="+value)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "BuildXL text requires valid UTF-8") {
				t.Fatalf("expected invalid UTF-8 diagnostic: %v\n%s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(dir, "fragment.dg"))
			if err != nil || string(data) != "previous fragment" {
				t.Fatalf("failed generation overwrote existing fragment: %q, %v", data, err)
			}
		})
	}
}

func TestBuildXLRejectUnsupported(t *testing.T) {
	cases := map[string]struct {
		makefile   string
		args       []string
		diagnostic string
	}{
		"phony-recipe":     {".PHONY: out\nout: ; @touch out\n", nil, "commandful phony"},
		"pattern-owner":    {"all: pair.first pair.second\n%.first %.second: %.in\n\t@touch $*.first $*.second\n", []string{"pair.first", "pair.second"}, "no declared output ownership"},
		"pattern-input":    {"out: pair.first pair.second\n\t@touch out\n%.first %.second: %.in\n\t@touch $*.first $*.second\n", nil, "no declared output ownership"},
		"depfile":          {"out: .KATI_DEPFILE = out.d\nout: ; @touch out\n", nil, "unsupported depfile"},
		"detect-depfile":   {"out: ; @touch out\n", []string{"--detect_depfiles"}, "does not support Ninja"},
		"timestamp":        {"out: ; @echo $? > out\n", nil, "timestamp-dependent"},
		"shell":            {"out: ; @echo $(shell touch side-effect) > out\n", nil, "deferred $(shell)"},
		"shell-export":     {"export DG_VALUE\nDG_VALUE = $(shell touch side-effect)\nout: ; @touch out\n", nil, "deferred $(shell)"},
		"realpath":         {"out: ; @echo $(realpath missing) > out\n", nil, "deferred $(realpath)"},
		"file-write":       {"out: ; @$(file >side-effect,text)touch out\n", nil, "$(file ...) is not supported"},
		"file-read":        {"out: ; @echo $(file <Makefile) > out\n", nil, "$(file ...) is not supported"},
		"cycle":            {"out: next\n\t@touch out\nnext: out\n\t@touch next\n", nil, "dependency cycle"},
		"double-colon":     {"out:: ; @touch out\n", nil, "double-colon"},
		"duplicate-owner":  {"all: out @ROOT@/out\nout @ROOT@/out: ; @touch $@\n", nil, "duplicate output ownership"},
		"directory-output": {"out/: ; @mkdir -p out\n", nil, "file outputs only"},
		"relative-shell":   {"SHELL = sh\nout: ; @touch out\n", nil, "absolute executable path"},
		"shell-quote":      {"SHELL = '/bin/bash\nout: ; @touch out\n", nil, "unterminated SHELL quote"},
		"shell-expansion":  {"SHELL = /bin/bash $$FLAGS\nout: ; @touch out\n", nil, "unsupported SHELL syntax"},
		"shell-operator":   {"SHELL = /bin/bash; touch side-effect\nout: ; @touch out\n", nil, "unsupported SHELL syntax"},
		"shell-flags":      {".SHELLFLAGS = -xc\nout: ; @touch out\n", nil, "custom .SHELLFLAGS"},
		"normalized-cycle": {"all: out next\nout: @ROOT@/next\n\t@touch out\nnext: @ROOT@/out\n\t@touch next\n", nil, "artifact dependency cycle"},
		"mutual-exclusion": {"out: ; @touch out\n", []string{"--ninja"}, "mutually exclusive"},
		"fragment-overlap": {"fragment.dg: ; @touch fragment.dg\n", nil, "filename overlaps"},
	}
	binary, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			dir := dgDir(t)
			dgWrite(t, dir, "Makefile", strings.ReplaceAll(test.makefile, "@ROOT@", dir))
			if name == "pattern-owner" || name == "pattern-input" {
				dgWrite(t, dir, "pair.in", "source\n")
			}
			cmd := exec.Command(binary, append([]string{"--buildxl=fragment.dg"}, test.args...)...)
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.diagnostic) {
				t.Fatalf("expected %q: %v\n%s", test.diagnostic, err, output)
			}
			if _, err := os.Stat(filepath.Join(dir, "side-effect")); !os.IsNotExist(err) {
				t.Fatal("rejected deferred IO ran during generation")
			}
			if _, err := os.Stat(filepath.Join(dir, "fragment.dg")); !os.IsNotExist(err) {
				t.Fatal("failed generation should not publish a partial fragment")
			}
		})
	}
}
