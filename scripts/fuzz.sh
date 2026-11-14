#!/usr/bin/env bash
# Runs all fuzz targets one after the other (invariant I5: no input crashes a parser).
#   bash scripts/fuzz.sh                       # all, FUZZTIME each (default 10s)
#   FUZZTIME=2m bash scripts/fuzz.sh internal/proto manifest
set -euo pipefail
cd "$(dirname "$0")/.."
FUZZTIME="${FUZZTIME:-10s}"
targets=()
while read -r file; do
  dir="./$(dirname "${file#./}")"
  while read -r fn; do
    targets+=("$dir $fn")
  done < <(grep -o '^func Fuzz[A-Za-z0-9_]*' "$file" | sed 's/^func //')
done < <(grep -rl --include='*_test.go' '^func Fuzz' "${@:-.}" | sort)
for t in "${targets[@]}"; do
  read -r dir fn <<<"$t"
  echo "== $dir $fn ($FUZZTIME)"
  go test -run '^$' -fuzz "^${fn}\$" -fuzztime "$FUZZTIME" "$dir"
done
echo "== ${#targets[@]} fuzz targets without findings"
