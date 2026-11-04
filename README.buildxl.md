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
arguments, an inherited action-environment baseline plus scoped exports,
declared file inputs/outputs, and `dependsOnCurrentOs`. Process fields are
`executable`, `workingDirectory`, repeated `argument`, repeated
`environment NAME VALUE`, repeated `option`, repeated `input`, repeated
`orderOnly`, and repeated `output`. `writeFile` has `destination` and `content`.
All file references are paths only. The consuming resolver matches local and
visible dependency outputs first; otherwise explicit file inputs are tracked
source files, like DScript. Directory grants require explicit directory inputs.
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

* Make extraction models declared file artifacts. Explicit directory grants and
  private writable roots can be supplied through the config below; hidden shell
  IO, recursive builds, directory membership and external side effects are not
  inferred. Consumed/published products still need actual declared producers. Existing
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

## Emission config

`--buildxl_config=FILE` (only with `--buildxl`) reads one caller-supplied config.
The pregraph generation pip **must declare FILE as a tracked input**, alongside
the Make evaluation inputs. Kati reads it; it does not discover providers,
publish exports, mutate earlier operations or add these fields in the loader.

```text
buildxlConfig 1
path 0 "/dependency/input"
path 1 "/dependency/include"
path 2 "/project/private-side-outputs"
string 0 "compiler-slots"
set 0 $p0
inheritEnvironment PATH HOME LC_ALL
passthroughEnvironment NJOBS MAKE_PARALL
process
  input @0
  inputDirectory $p1 "dependency/headers"
  outputDirectory $p2 shared
  option keepOutputsWritable
  semaphore $s0 4 1
  enableTempDirectory true
```

The config uses UTF-8, full-line `#` comments and the text format's bare tokens
and quoted literals. Supported quoted escapes are `\\`, `\"`, `\n`, `\r`, `\t`;
use literal UTF-8 rather than Unicode escape sequences. NUL, malformed tokens,
unknown records/fields and invalid references are errors, with config record
locations. Table records precede the single optional `process` section.
Paths are normalized against Kati's evaluation directory. Supplied IDs are
dense and stable, including repeated values. Kati emits all supplied tables
itself and independently starts generated path/string/set IDs after each prefix;
IDs never reference another fragment's tables.

Initial process additions are `input`, `orderOnly` (path references or sets),
`inputDirectory` (path and optional producer, including `project/operation`),
`outputDirectory` (path and optional `shared`/`exclusive`, default shared),
`option`, `semaphore` (string name, positive decimal limit/increment),
`tempDirectory` (path) and `enableTempDirectory` (`true`/`false`). They apply to
every process, **not** generated-script `writeFile` records. Kati owns executable,
arguments, file outputs and rewrites. Raw `environment`/`passthroughEnvironment`
process additions and all other fields are rejected.

Duplicate output-directory roots, semaphore names, options or singleton temp
settings are errors. Enabled automatic and explicit temp settings conflict.
Multiple actions cannot share an explicit temp or exclusive output directory;
use native automatic per-process temp and shared writable roots instead.
Additional prerequisites cannot name any output (including lowered scripts) of
this fragment, even a later one, or files under its uniform writable roots.
Input directories cannot overlap those writable roots. Such roots do not merge
writers or publish an SDK view: consumed generated products still need explicit
file edges or producer-qualified directory dependencies/compositions.

With a config, the inherited action baseline contains only names selected by
top-level `inheritEnvironment NAME...` records (none by default). These select
names, never values; missing inherited names remain absent. Make evaluation
still sees the full invocation environment. Ordinary names then receive Kati's
existing scoped Make export/unexport overlay: explicit exports can introduce
unselected names, unexports remove names, and a non-exported reassignment does
not replace an inherited baseline value. Without a config, the legacy inherited
baseline remains available. In both modes `TEMP`, `TMP`, `TMPDIR` are excluded
from inheritance; explicitly selecting them is an error. Use native process
temp construction rather than capturing the pregraph pip's temp paths.

Top-level `passthroughEnvironment NAME...` metadata is authoritative for the
selected names (for example `NJOBS`, `MAKE_PARALL`). Each name emits exactly one
process `passthroughEnvironment $sID`, in stable name order, even when absent,
assigned, exported, unexported or target-scoped in Make. No tracked value or
fallback is captured, and deferred export values need not be evaluated just to
populate these fields. Ordinary export precedence remains unchanged for other
names. Duplicate/invalid names are errors; overlapping inheritance selection is
allowed but passthrough wins. This metadata is distinct from forbidden raw
process environment rows.

Only runtime uses such as `$$NJOBS` remain late-bound. `$(NJOBS)` already expanded
into command text remains captured and affects the action definition. Passthrough
does not change pregraph evaluation tracking or recompute derived `MAKE_PARALL`.

An earlier ordinary `graphFiles` fragment can construct an input seal referenced
by the config's `inputDirectory`; a dependency directory can be selected by its
producer. Trailing ordinary policy fragments can construct seals/compositions
and exports from actual outputs. No phase defaults or membership scans occur.

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

Add `--configured` to exercise shared config prefixes, automatic temp, a mutex,
shared private side-output roots, an earlier local source-seal fragment and a
producer-qualified `tools/hostUsr` directory input. Prepend the emitted
`producer-inputs.dg`/`consumer-inputs.dg` to the respective `graphFiles`.
The optional BuildXL regression detects and loads these earlier fragments.
This fixture still runs Kati outside BuildXL; it does not claim pregraph-pip
tracking or real product compatibility validation.

## SDK Make compatibility inventory (read-only, 2026-11-04)

Inspection covered 171 installed Make/`.mk`/`.mak` files under the SDK delivery
tree's `FunSDK`, `FunDoc`, `lib`, `scripts` and `integration_test` directories.
No SDK file was modified and no SDK/BoB graph or recipe was generated/executed.
These are extraction requirements, not product compatibility results:

* **FORCE:** `co3-build-inputs.mk`'s `co3-build-inputs-rule` depends on the
  commandless `co3-build-inputs-force` and writes change-only environment content.
  `lib/krml/Makefile` uses `.FORCE` for `.depend`. The exporter flattens these
  structural aliases; it does not emulate unconditional Make timestamps.
  Content-addressed reuse is appropriate only with actual evaluation/action
  inputs tracked. Do not blanket-disable caching for FORCE.
* **Recursive Make:** `co3-prebuild.mk`, `co3-vars.mk`, `co3-postbuild.mk`,
  `co3-package-helper.mk` and `lib/krml/Makefile` invoke `$(MAKE)`.
  `co3-cap-gen` deliberately relies on targets available only in sub-makes.
  Kati does not extract child build graphs or hidden products; commandful phony
  actions are rejected. Integration must explicitly select/lower child actions
  or use properly declared coarse workers. Generated includes/extraction
  lifecycle also need deliberate pregraph ordering.
* **Grouped/implicit products:** `lib/krml/Makefile` has three `&:` pairs for
  `Makefile.include`/`Makefile.basic`; the grouped-owner implementation covers
  these declarations. Their recipes also emit/copy headers and archives.
  Co3 extraction similarly has a combined-JSON owner, a value-JSON byproduct
  and existence-only assertion recipes; split-header wildcards can be empty in
  clean trees. `Phase.Target.co3.io` explicitly describes directory-shaped
  output with names not derivable from input patterns. No
  `.KATI_IMPLICIT_OUTPUTS` declarations were found in this bounded inventory.
  These hidden products need real declared file ownership or explicit dynamic
  directory providers; uniform private writable roots alone do not publish
  them. Do not discover generated membership by scanning existing outputs.
* **Compiler side outputs:** `Tool.gcc.1.mkflags_recipe` emits `-MMD/-MP/-MF`;
  `tools-gcc.mk`, `tools-clang.mk` and `tools-gccmips64.mk` also produce depfiles.
  The exporter does not infer dependencies/products from them and rejects
  explicit `.KATI_DEPFILE`/automatic Ninja depfile options. Callers must supply
  explicit file/directory grants and writable side-output roots as appropriate.
* **Symlinks:** `co3-package-helper.mk`'s image-stamp recipe uses `ln -sf`;
  `Phase.Target.mkpkg` uses `BuildRoot.ln`, and the FunOSPosix FIPS toolchain
  declares `Tool.ln.1.fipssec`. Existing symlink components in artifact paths
  are rejected by the exporter. It does not infer symlink targets or extra
  accesses from shell commands. Generated-link sandbox/cache behavior and
  declarations remain integration obligations, not newly validated support.

No additional Make capability or blanket cache policy was justified by this
read-only inventory. Real consumer validation, including stricter source seals,
wrapper job-control forwarding and BoB trial wiring, remains separately gated.
