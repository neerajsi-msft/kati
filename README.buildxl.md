# BuildXL export (experimental C++ backend)

`./ckati --buildxl=project.dg [goals...]` emits a resolved action graph for
BuildXL in its DirectGraph text format, without generating or parsing Ninja and
without executing recipes. This option is mutually exclusive with `--ninja`.
The compact streaming text contract starts with `directgraph 2` (regenerate
version 1 fragments):

```text
directgraph 2
path 0 "/absolute/evaluation/directory"
path 1 "/absolute/source/input"
path 2 "/bin/bash"
path 3 "/absolute/evaluation/directory/output"
string 0 "-c"
string 1 "cat /absolute/source/input > output"
string 2 "PATH"
string 3 "/usr/bin:/bin"
set 0 $p1
process action_0123456789abcdef
  executable $p2
  workingDirectory $p0
  argument $s0
  argument $s1
  environment $s2 $s3
  option dependsOnCurrentOs
  input @0
  output $p3
```

UTF-8 records are written incrementally to the output stream, not collected as
an encoded whole-graph document. Fields are indented by two spaces, with one
field per line. All path/string fields require typed intern references, matching
the gRPC interface's ID-valued fields, even for values used once. Operation names
and enum/option values remain literal.
Quoted tokens escape `\\`, `\"`, `\n`, `\r`, `\t`, and other ASCII controls
with `\uXXXX`; they never expand environment variables. Literals must be valid
UTF-8; invalid values are rejected before writing the fragment. Blank lines and
full-line `#` comments are permitted. A top-level record or EOF ends a field
block. `path ID "literal"` and `string ID "literal"` define immutable,
fragment-local values referenced by `$pID` and `$sID`. Each table uses dense
integers starting at zero in registration order. Definitions precede uses and
are never inserted inside field blocks.

Operations are topologically ordered `process` or `writeFile` records.
Processes use Kati's shell (`SHELL` must start with an absolute executable path;
literal options, quoting and escaping are supported, but shell expansions and
operators are rejected), literal
arguments, an explicit evaluation-environment snapshot plus scoped exports,
declared file inputs/outputs, and `dependsOnCurrentOs`. Process fields are
`executable`, `workingDirectory`, repeated `argument`, repeated
`environment NAME VALUE`, repeated `option`, repeated `input`, repeated
`orderOnly`, and repeated `output`. `writeFile` has `destination` and `content`.
All file references are paths only. The consuming resolver matches local and
visible dependency outputs first; other inputs require an explicit source seal.
Kati emits absolute, lexical-normalized paths, so there is no root record.
Kati emits **no exports**: project configuration or policy fragments select
recipe outputs, including grouped and `.KATI_IMPLICIT_OUTPUTS` members.
The consuming executor must reject missing declared outputs after execution.
Operation names use stable hashes with collision guards. Intern and dependency-set
IDs are dense and fragment-local, not Kati's global symbol IDs.

An `argument $s0 $s1 ...` field concatenates its string references with no
inserted characters into one argument; the single-reference form remains valid.
The exporter factors arguments into whitespace runs, individual double/single
quotes and backticks, and intervening literal runs, retaining every character
exactly and without interpreting shell syntax. The text
loader preserves factoring in binary `PipData` while escaping the complete
argument, not the individual original chunks. This text-only extension does not
change the gRPC contract. A `writeFile`'s single `content` field likewise accepts
multiple string references, concatenated with no escaping or inserted separators.
Generated scripts use the same chunk boundaries as arguments and retain their
factoring in binary `PipData`. Environment values remain whole interned strings.

All evaluated commandless rules become structural aliases, matching Kati-to-Ninja
phony-edge semantics: normal edges retain normal intent;
order-only edges, including all prerequisites beneath an order-only alias,
remain order-only. Repeated **fully evaluated exact lists** share a
`set ID $pID...` record, preserving order and multiplicity.
This is textual sharing only, not variable provenance, shared evaluation, a
new action, or a native scheduling barrier. Sets contain path references,
never nested sets, and are referenced only by `input @ID` or
`orderOnly @ID`; unshared inputs are emitted as individual fields.
Order-only inputs represent scheduling/existence intent only, not permission
to read or a content fingerprint; source order-only paths are checked/tracked
when the consuming resolver loads the graph. Files read by recipes must also be
normal prerequisites (or covered by a consuming loader's explicit directory
input contract).
Commandless rules create no process or output file; their consumers inherit
their recursively flattened prerequisites. Input-free commandless rules contribute
no input, regardless of whether their named file exists. Missing files without
a rule remain source prerequisites, not empty aliases.

The existing `CommandEvaluator` handles target scopes, automatic variables,
recipe prefixes, and delayed diagnostics. Each recipe line runs in a separate
shell using Kati's existing `-c` (or `.POSIX` `-ec`) behavior; ignored failures
do not stop later lines. Raw scripts do not contain Ninja's doubled dollar
encoding. Commands above the conservative 100 KB argv threshold, or combined
recipes exceeding it, lower to declared `writeFile` scripts under
`<fragment-filename>.scripts/`, consumed by the process. Emission does not create
these scripts; the executor creates them. The fragment's parent directory must
already exist.

Bounded limitations:

* Only declared file artifacts are modeled. Recipes must declare all reads and
  writes through prerequisites and outputs; hidden shell IO, recursive builds,
  directory artifacts and external side effects are not inferred. Existing
  symlink components in artifact paths are rejected (shell executables may
  be symlinks). Absolute paths are lexical-normalized against the evaluation cwd;
  relocation/source-root sealing is not provided by Kati.
* Phony aliases do not carry GNU Make's unconditional timestamp invalidation.
  The flattened dependency model does not emulate Ninja's phony mtime/dirty-state
  checks, including invalidation caused by a missing input-free phony output.
  Commandful phony targets, `$?`, dependency cycles (including normalized aliases),
  and conflicting output ownership are rejected rather than represented as
  cacheable processes. Implicit multi-output pattern siblings without declared
  ownership are rejected when requested or consumed; use explicit grouped
  rules or `.KATI_IMPLICIT_OUTPUTS` for shared output ownership.
* Depfiles (`.KATI_DEPFILE` or `--detect_depfiles`), validations, Ninja pools/tags,
  Android command rewriting, double-colon rules, and custom `.SHELLFLAGS` are
  rejected.
* Special execution/lifetime declarations follow existing Kati-to-Ninja
  semantics, not GNU Make semantics. `.SECONDARY`, `.PRECIOUS`, `.INTERMEDIATE`,
  `.DELETE_ON_ERROR`, `.ONESHELL`, `.IGNORE`, `.SILENT`, `.NOTPARALLEL`,
  `.EXPORT_ALL_VARIABLES`, and `.LOW_RESOLUTION_TIME` do not change the exported
  graph; existing Kati warnings are retained. Intermediate outputs are not
  automatically deleted, and GNU Make's missing-secondary-file timestamp
  optimization is not modeled. Output caching and failure cleanup belong to
  the executor.
* Recipe-time/deferred `$(shell)`, `$(KATI_shell_no_rerun)`, `$(realpath)`, and
  `$(file ...)` IO is rejected explicitly, including deferred exported values.
  No runtime substitution is silently frozen into literal environment
  values. Existing parse/evaluation-time IO still happens normally, including
  Kati's eager expansion of inline `export NAME = ...` declarations; emission
  is not a hermetic evaluation engine. Delayed `$(info)`, `$(warning)`, and
  `$(error)` commands remain in the emitted action.
* This backend does not emit a project manifest or link separate fragments.
  A separate loader resolves input paths through dependency
  projects' exports and applies project namespacing and caching.

Build with `make ckati`, then run
`go test -run 'TestBuildXL|TestGroupedTargets'`.
BuildXL exporter tests include a deterministic one-shot contract/execution harness,
not a BuildXL cache simulation or integration test.
`TestBuildXLGeneratedHeaderCompile` also executes a real C compiler with a
normally declared generated-header producer input (skipped only if `cc` is
unavailable). This checks host recipe execution, not BuildXL sandbox/toolchain
authorization.

## Reproducing the two-project BuildXL integration fixture

From this checkout on Linux, generate into a **new** isolated directory:

```sh
make ckati
python3 examples/buildxl/generate_fixture.py /absolute/path/to/new-fixture
```

The helper uses this checkout's `ckati` with a minimal environment, writes
producer/consumer Makefiles, and emits root-level `producer.dg`, `consumer.dg`,
`tools.dg`, and human-readable `projects.txt` configuration metadata.
`tools.dg` exports recursive source-directory seals for `/usr` and lexical
`/bin` (including symlinked shell paths). It does not seal the fixture root or
generated output directories. Configure `producer` to depend on `tools`, and
`consumer` to depend directly on both `producer` and `tools`. Export
`producer/generated.h` and `producer/generated-input.txt` from `producer` using
project configuration; optionally export `consumer/consumer.out` from `consumer`.
The helper executes no recipes and refuses existing
destinations: no outputs are deleted or overwritten. Point the optional BuildXL
regression at `KATI_DIRECTGRAPH_FIXTURE=/absolute/path/to/new-fixture`; execution
should produce `consumer/consumer.out` containing
`#define GENERATED_VALUE 37\ndirectgraph-input\n`.
Metadata paths and Makefiles are relative; fragments use absolute evaluation
paths, so regenerate rather than copying fragments to another root.
