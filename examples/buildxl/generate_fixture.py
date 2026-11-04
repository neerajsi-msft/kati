#!/usr/bin/env python3
# Copyright 2026 Microsoft Corporation
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# https://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Generate BuildXL fixtures in DirectGraph format without executing recipes."""

import argparse
import json
import os
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", help="new isolated destination directory")
    parser.add_argument("--configured", action="store_true", help="exercise config prefixes and cross-fragment process inputs")
    args = parser.parse_args()
    destination = Path(args.directory).resolve()
    kati = Path(__file__).resolve().parents[2] / "ckati"
    if not kati.is_file() or not os.access(kati, os.X_OK):
        parser.error("build this checkout's ckati with 'make ckati' first")
    try:
        destination.mkdir(parents=True, exist_ok=False)
    except FileExistsError:
        parser.error("destination already exists; choose a new isolated directory")
    except OSError as error:
        parser.error(str(error))

    makefiles = {
        "producer": (
            ".PHONY: all\n"
            "all: generated.h generated-input.txt\n\n"
            "generated.h generated-input.txt &:\n"
            "\t@printf '%s\\n' '#define GENERATED_VALUE 37' > generated.h\n"
            "\t@printf '%s\\n' 'directgraph-input' > generated-input.txt\n"
        ),
        "consumer": (
            ".PHONY: all\n"
            "all: consumer.out\n\n"
            "consumer.out: ../producer/generated.h ../producer/generated-input.txt\n"
            "\t@cat ../producer/generated.h ../producer/generated-input.txt > consumer.out\n"
        ),
    }
    scratch = destination / "compiler-scratch"
    scratch.mkdir()
    environment = {
        "PATH": "/usr/bin:/bin",
        "HOME": str(destination),
        "TMPDIR": str(scratch),
        "LC_ALL": "C",
    }
    config_args = []
    if args.configured:
        inputs = destination / "policy-inputs"
        inputs.mkdir()
        (inputs / "extra").write_text("extra tracked source\n", encoding="utf-8")
        config = destination / "emission.config"
        config.write_text(
            "buildxlConfig 1\n"
            + "".join(f"path {i} {json.dumps(str(path))}\n" for i, path in enumerate([
                inputs, Path("/usr"), inputs / "extra", destination / "private-side",
            ]))
            + 'string 0 "fixture-mutex"\nset 0 $p2\n'
            + 'inheritEnvironment PATH HOME LC_ALL\n'
            + 'passthroughEnvironment NJOBS MAKE_PARALL\nprocess\n'
            + '  input @0\n  inputDirectory $p0 inputs\n'
            + '  inputDirectory $p1 "tools/hostUsr"\n'
            + '  outputDirectory $p3 shared\n  semaphore $s0 1 1\n'
            + '  enableTempDirectory true\n',
            encoding="utf-8",
        )
        config_args = ["--buildxl_config=" + str(config)]
        for project in makefiles:
            (destination / (project + "-inputs.dg")).write_text(
                f"directgraph 2\npath 0 {json.dumps(str(inputs))}\n"
                'string 0 ".git"\nsealDirectory inputs\n'
                '  directoryRoot $p0\n  sealKind sourceAllDirectories\n'
                '  sourceExclusion $s0\n',
                encoding="utf-8",
            )
    for project, makefile in makefiles.items():
        if args.configured:
            makefile = makefile.replace(
                "\t",
                '\t@test -n "$$TMPDIR"; printf private > "$$TMPDIR/private"\n\t',
                1,
            )
        directory = destination / project
        directory.mkdir()
        (directory / "Makefile").write_bytes(makefile.encode("utf-8"))
        subprocess.run(
            [
                str(kati),
                "--no_builtin_rules",
                "--buildxl=" + str(destination / (project + ".dg")),
                *config_args,
                "all",
            ],
            cwd=directory,
            env=environment,
            check=True,
        )
    (destination / "tools.dg").write_text(
        'directgraph 2\n'
        'path 0 "/usr"\n'
        'path 1 "/bin"\n'
        'sealDirectory hostUsr\n'
        '  directoryRoot $p0\n'
        '  sealKind sourceAllDirectories\n'
        'sealDirectory hostBin\n'
        '  directoryRoot $p1\n'
        '  sealKind sourceAllDirectories\n'
        'export hostExports\n'
        '  directory $p0\n'
        '  directory $p1\n',
        encoding="utf-8",
    )
    (destination / "projects.txt").write_text(
        "root consumer\n"
        "project tools tools.dg\n"
        "project producer producer.dg depends tools\n"
        "project consumer consumer.dg depends tools producer\n"
        "export producer file producer/generated.h\n"
        "export producer file producer/generated-input.txt\n"
        "export consumer file consumer/consumer.out\n",
        encoding="utf-8",
    )
    if args.configured:
        (destination / "configured.txt").write_text(
            "Prepend producer-inputs.dg and consumer-inputs.dg to the respective graphFiles.\n"
            "Declare emission.config as a tracked input of each pregraph Kati pip.\n",
            encoding="utf-8",
        )
    print(destination / "projects.txt")


if __name__ == "__main__":
    main()
