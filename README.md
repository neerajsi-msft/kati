# kati

[![Build and Test](https://github.com/google/kati/workflows/Build%20and%20Test/badge.svg)](https://github.com/google/kati/actions)

kati is an experimental GNU make clone. The main goal of this tool is to
speed-up incremental build of Android.

### Explicit grouped targets (local extension)

Explicit `first second &: inputs` rules are lowered to one action, using the
same output ownership machinery as `.KATI_IMPLICIT_OUTPUTS`. Either member can
be requested; missing or stale members rebuild the group. Additional ordinary
prerequisites and target-specific variables on members are merged as for Kati
implicit outputs. Ninja receives `build first | second: ...`.

This is a bounded subset of GNU Make 4.3 grouped targets. Kati always evaluates
automatic variables such as `$@` using the first listed output, regardless of
which member was requested. Recipes must not depend on GNU Make's
triggering-member choice. Pattern/static-pattern/suffix or phony groups, `&::`, grouped
assignments, overlapping groups, and recipe replacement are rejected.
Existing `.KATI_IMPLICIT_OUTPUTS` declarations remain supported.
The built-in `KATI_GROUPED_TARGETS` variable is `explicit`, allowing callers to
require this extension without relying on `MAKE_VERSION` (still 4.2.1).

Run `go test -run TestGroupedTargets` after building `ckati` to check the direct
executor, Ninja export, incremental repair, and unsupported-form diagnostics.

Currently, kati does not offer a faster build by itself. It instead converts
your Makefile to a ninja file.

## Development

Building:

```
$ make ckati
```

The above command produces a `ckati` binary in the project root.

Testing (best ran in a Ubuntu 22.04 environment):

```
$ make test
$ go test --ckati
$ go test --ckati --ninja
$ go test --ckati --ninja --all
```

The above commands run all cKati and Ninja tests in the `testcases/` directory.

Alternatively, you can also run the tests in a Docker container in a prepared
test enviroment:

```
$ docker build -t kati-test . && docker run kati-test
```

If you are working on a machine that does not provide `make` in the same version
as kati is currently compatible with, you might want to download a prebuilt
version instead. For example to use the prebuilt version of Ubuntu 20.04 LTS:

```
  $ mkdir tmp/ && cd tmp/
  $ wget http://mirrors.kernel.org/ubuntu/pool/main/m/make-dfsg/make_4.2.1-1.2_amd64.deb
  $ ar xv make_4.2.1-1.2_amd64.deb
  $ tar xf data.tar.xz
  $ cd ..
  $ PATH=$(pwd)/tmp/usr/bin/:$PATH make test
```

## How to use for Android

For Android-N+, ckati and ninja is used automatically. There is a prebuilt
checked in under prebuilts/build-tools that is used.

All Android's build commands (m, mmm, mmma, etc.) should just work.

## Rust version

There's an experimental rust port in the the src-rs/... directory, though there
are no plans for it to replace the C++ version. It should have a very similar
feature set to the C++ version, and similar performance (in release mode).

```
$ cargo build
$ go test --rkati
$ go test --rkati --ninja
$ go test --rkati --ninja --all
```

The resulting `rkati` binary in `target/*/rkati` is a drop-in replacement for
`ckati`. Setting `KATI_LOG=trace` at runtime can enable extra debug logging.