#!/usr/bin/env bash
# Copyright 2026 The HAMi Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Usage: verify-elf-arch.sh <root-dir> <amd64|arm64>
# Fails if any ELF file under <root-dir> is built for another architecture.

set -euo pipefail

root="$1"
# ELF e_machine values (little-endian, at offset 18).
case "$2" in
    amd64) want=62 ;;   # EM_X86_64
    arm64) want=183 ;;  # EM_AARCH64
    *) echo "unsupported architecture: $2" >&2; exit 2 ;;
esac

checked=0
mismatched=0
while IFS= read -r -d '' file; do
    read -r -a b <<<"$(od -An -v -tu1 -N20 -- "${file}" | tr -s ' \n' ' ')"
    if [[ ${#b[@]} -lt 20 || "${b[*]:0:4}" != "127 69 76 70" ]]; then
        continue
    fi
    checked=$((checked + 1))
    machine=$((b[18] + b[19] * 256))
    if [[ ${machine} -ne ${want} ]]; then
        echo "wrong architecture: ${file#"${root%/}"} has e_machine ${machine}, want ${want} ($2)" >&2
        mismatched=$((mismatched + 1))
    fi
done < <(find "${root}" -xdev -type f -print0)

if [[ ${checked} -eq 0 ]]; then
    echo "no ELF files found under ${root}" >&2
    exit 1
fi
if [[ ${mismatched} -ne 0 ]]; then
    echo "${mismatched} of ${checked} ELF files are not $2" >&2
    exit 1
fi
echo "all ${checked} ELF files are $2"
