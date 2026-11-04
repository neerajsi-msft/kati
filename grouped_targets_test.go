// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kati

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func groupedTestDir(t *testing.T) string {
	t.Helper()
	dir, err := ioutil.TempDir(".", "kati-grouped-")
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

func TestGroupedTargets(t *testing.T) {
	kati, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"direct", "ninja", "make"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "make" {
				cmd := exec.Command("make", "-sf", "/dev/null",
					"--eval=$(info $(filter grouped-target,$(.FEATURES)))")
				output, _ := cmd.CombinedOutput()
				if !strings.Contains(string(output), "grouped-target\n") {
					t.Skip("GNU Make 4.3+ required for grouped-target reference")
				}
			}
			dir := groupedTestDir(t)
			write := func(name, contents string) {
				t.Helper()
				if err := ioutil.WriteFile(filepath.Join(dir, name), []byte(contents), 0644); err != nil {
					t.Fatal(err)
				}
			}
			run := func(program string, args ...string) string {
				t.Helper()
				cmd := exec.Command(program, args...)
				cmd.Dir = dir
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s %v: %v\n%s", program, args, err, output)
				}
				return string(output)
			}
			write("Makefile", "members := first second\nall: first second\n"+
				"first: normal\nsecond: extra | ordered\n"+
				"$(members)&: input\n"+
				"\t@echo invocation >> calls\n"+
				"\t@echo $@ > automatic\n"+
				"\t@cat input normal extra > first\n"+
				"\t@cp first second\n"+
				"consumer: second\n\t@cp second consumer\n"+
				"ordered-consumer: | second\n\t@echo invocation >> ordered-calls\n\t@touch ordered-consumer\n"+
				"independent-a independent-b:\n\t@echo $@ >> independent\n")
			for _, name := range []string{"input", "normal", "extra", "ordered"} {
				write(name, name+"\n")
			}
			build := func(goals ...string) {
				t.Helper()
				if backend == "ninja" {
					run(kati, append([]string{"--ninja", "--gen_all_targets"}, goals...)...)
					graph, err := ioutil.ReadFile(filepath.Join(dir, "build.ninja"))
					if err != nil || !strings.Contains(string(graph), "build first | second:") {
						t.Fatalf("expected one grouped edge: %v\n%s", err, graph)
					}
					run("ninja", append([]string{"-j8"}, goals...)...)
				} else if backend == "make" {
					run("make", append([]string{"-s", "-j8"}, goals...)...)
				} else {
					run(kati, goals...)
				}
			}
			check := func(calls int) {
				t.Helper()
				data, err := ioutil.ReadFile(filepath.Join(dir, "calls"))
				if err != nil || strings.Count(string(data), "invocation\n") != calls {
					t.Fatalf("expected %d invocations: %v\n%s", calls, err, data)
				}
				for _, name := range []string{"first", "second"} {
					data, err = ioutil.ReadFile(filepath.Join(dir, name))
					if err != nil || string(data) != "input\nnormal\nextra\n" {
						t.Fatalf("%s: %v\n%s", name, err, data)
					}
				}
			}
			build("second")
			check(1)
			data, err := ioutil.ReadFile(filepath.Join(dir, "automatic"))
			expectedAutomatic := "first\n"
			if backend == "make" {
				expectedAutomatic = "second\n"
			}
			if err != nil || string(data) != expectedAutomatic {
				t.Fatalf("expected canonical $@: %v\n%s", err, data)
			}
			build("all")
			check(1)
			for i, name := range []string{"first", "second"} {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
				build(name)
				check(i + 2)
			}
			// Changing a sibling's additional prerequisite rebuilds the group.
			future := time.Now().Add(2 * time.Second)
			if err := os.Chtimes(filepath.Join(dir, "extra"), future, future); err != nil {
				t.Fatal(err)
			}
			build("first")
			check(4)
			past := time.Now().Add(-10 * time.Second)
			if err := os.Chtimes(filepath.Join(dir, "extra"), past, past); err != nil {
				t.Fatal(err)
			}
			build("consumer")
			write("input", "changed\n")
			if err := os.Remove(filepath.Join(dir, "second")); err != nil {
				t.Fatal(err)
			}
			build("consumer")
			data, err = ioutil.ReadFile(filepath.Join(dir, "consumer"))
			if err != nil || string(data) != "changed\nnormal\nextra\n" {
				t.Fatalf("dependent must see repaired grouped output: %v\n%s", err, data)
			}
			build("ordered-consumer")
			if err := os.Remove(filepath.Join(dir, "second")); err != nil {
				t.Fatal(err)
			}
			build("ordered-consumer")
			data, err = ioutil.ReadFile(filepath.Join(dir, "ordered-calls"))
			if err != nil || string(data) != "invocation\n" {
				t.Fatalf("order-only repair must not rebuild consumer: %v\n%s", err, data)
			}
			data, err = ioutil.ReadFile(filepath.Join(dir, "second"))
			if err != nil || string(data) != "changed\nnormal\nextra\n" {
				t.Fatalf("order-only dependency must repair grouped output: %v\n%s", err, data)
			}
			build("independent-a", "independent-b")
			data, err = ioutil.ReadFile(filepath.Join(dir, "independent"))
			if err != nil || !strings.Contains(string(data), "independent-a\n") ||
				!strings.Contains(string(data), "independent-b\n") {
				t.Fatalf("ordinary multi-target rules must remain independent: %v\n%s", err, data)
			}
		})
	}
}

func TestGroupedTargetsDefaultGoal(t *testing.T) {
	kati, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	for _, ninjaMode := range []bool{false, true} {
		for _, targets := range []string{"first", "first second"} {
			mode := "-direct"
			if ninjaMode {
				mode = "-ninja"
			}
			t.Run(targets+mode, func(t *testing.T) {
				dir := groupedTestDir(t)
				contents := targets + " &: ; @touch " + targets + "\n"
				if err := ioutil.WriteFile(filepath.Join(dir, "Makefile"), []byte(contents), 0644); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(kati)
				if ninjaMode {
					cmd.Args = append(cmd.Args, "--ninja")
				}
				cmd.Dir = dir
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("default goal: %v\n%s", err, output)
				}
				if ninjaMode {
					cmd = exec.Command("ninja", "-j8")
					cmd.Dir = dir
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("Ninja default goal: %v\n%s", err, output)
					}
				}
				for _, target := range strings.Fields(targets) {
					if _, err := os.Stat(filepath.Join(dir, target)); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestGroupedTargetsIgnoreMemberImplicitRules(t *testing.T) {
	kati, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"direct", "ninja", "buildxl"} {
		for _, order := range []string{"a.o b.o", "b.o a.o"} {
			for _, rule := range []string{"suffix", "pattern"} {
				t.Run(backend+"/"+order+"/"+rule, func(t *testing.T) {
					dir := groupedTestDir(t)
					makefile := "all: " + order + "\na.o b.o &: a.c b.c\n" +
						"\t@echo grouped >> calls\n\t@cat a.c > a.o\n\t@cat b.c > b.o\n"
					if rule == "pattern" {
						makefile += "%.o: %.c\n\t@echo implicit >> calls\n\t@cp $< $@\n"
					}
					dgWrite(t, dir, "Makefile", makefile)
					dgWrite(t, dir, "a.c", "first\n")
					dgWrite(t, dir, "b.c", "second\n")
					run := func(program string, args ...string) {
						t.Helper()
						cmd := exec.Command(program, args...)
						cmd.Dir = dir
						if output, err := cmd.CombinedOutput(); err != nil {
							t.Fatalf("%s %v: %v\n%s", program, args, err, output)
						}
					}
					switch backend {
					case "direct":
						run(kati, "all")
					case "ninja":
						run(kati, "--ninja", "all")
						run("ninja", "-j2", "all")
					case "buildxl":
						g, _ := dgGenerate(t, dir, "all")
						if err := dgExecute(t, g); err != nil {
							t.Fatal(err)
						}
					}
					for name, expected := range map[string]string{
						"calls": "grouped\n", "a.o": "first\n", "b.o": "second\n",
					} {
						data, err := ioutil.ReadFile(filepath.Join(dir, name))
						if err != nil || string(data) != expected {
							t.Fatalf("%s: got %q, want %q, error %v", name, data, expected, err)
						}
					}
				})
			}
		}
	}
}

func TestGroupedTargetsRejectUnsupportedForms(t *testing.T) {
	kati, err := filepath.Abs("ckati")
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct{ rule, diagnostic string }{
		"double-colon": {"a b &::\n\ttrue\n", "explicit &: rules only"},
		"empty":        {"&:\n\ttrue\n", "explicit &: rules only"},
		"pattern":      {"%.a %.b &: %.in\n\ttrue\n", "explicit &: rules only"},
		"suffix":       {".a.b &:\n\ttrue\n", "grouped suffix"},
		"phony":        {".PHONY: b\na b &:\n\ttrue\n", "phony grouped"},
		"static":       {"a b &: %: %.in\n\ttrue\n", "static pattern"},
		"no-recipe":    {"a b &:\n", "must provide a recipe"},
		"assignment":   {"a b &: VAR=value\n", "assignments"},
		"duplicate":    {"a a &:\n\ttrue\n", "duplicate grouped target"},
		"overlap":      {"a b &:\n\ttrue\nc b &:\n\ttrue\n", "already defined"},
		"override":     {"a b &:\n\ttrue\na:\n\ttrue\n", "overriding grouped target"},
		"prior-recipe": {"b:\n\ttrue\na b &:\n\ttrue\n", "may not have commands"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			dir := groupedTestDir(t)
			if err := ioutil.WriteFile(filepath.Join(dir, "Makefile"), []byte(test.rule), 0644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(kati, "--ninja")
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.diagnostic) {
				t.Fatalf("expected rejection containing %q: %v\n%s", test.diagnostic, err, output)
			}
		})
	}
}
