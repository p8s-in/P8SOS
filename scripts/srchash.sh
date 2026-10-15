#!/usr/bin/env bash
# Fingerprint of the P8S OS source that p8sd and its tools are built from.
# The installer ISO stores it next to its pre-built programs (prebuilt/);
# install.sh uses those programs only when the fingerprint still matches
# (after an update the source differs, so it builds again).
#   bash scripts/srchash.sh [REPO]
set -euo pipefail
cd "${1:-$(dirname "${BASH_SOURCE[0]}")/..}"
{ find cmd internal docs -type f ! -name '*_test.go' ! -name 'zz_*' 2>/dev/null
  for f in go.mod go.sum VERSION; do if [[ -f $f ]]; then echo "$f"; fi; done
} | LC_ALL=C sort | while IFS= read -r f; do printf '%s  %s\n' "$(sha256sum < "$f" | cut -c1-64)" "$f"; done | sha256sum | cut -c1-64
