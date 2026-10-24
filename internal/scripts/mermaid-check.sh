#!/usr/bin/env bash
# Renders every ```mermaid block in the repository's GitHub-rendered
# Markdown with the real Mermaid CLI, so a diagram GitHub cannot parse
# (a ";" in a message ends the statement, for one) fails here first.
# Needs Node (npx downloads @mermaid-js/mermaid-cli on first use).
set -euo pipefail
cd "$(dirname "$0")/../.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
python3 - "$tmp" <<'PY'
import glob, sys
out = sys.argv[1]
files = ['README.md'] + glob.glob('docs/**/*.md', recursive=True) + glob.glob('**/README.md', recursive=True)
n = 0
for f in sorted(set(files)):
    if f.startswith(('docsi', '.lore', 'node_modules')):
        continue
    buf = None
    for i, line in enumerate(open(f).read().split('\n'), 1):
        if line.strip() == '```mermaid':
            buf, start = [], i
        elif buf is not None and line.strip() == '```':
            n += 1
            open(f"{out}/{n:03d}.mmd", 'w').write('\n'.join(buf))
            open(f"{out}/{n:03d}.src", 'w').write(f"{f}:{start}")
            buf = None
        elif buf is not None:
            buf.append(line)
PY
fail=0
for m in "$tmp"/*.mmd; do
  [ -e "$m" ] || continue
  if ! npx -y -p @mermaid-js/mermaid-cli@11 mmdc -q -i "$m" -o "${m%.mmd}.svg" >/dev/null 2>"${m%.mmd}.err"; then
    echo "FAIL $(cat "${m%.mmd}.src")"; head -5 "${m%.mmd}.err"; fail=1
  fi
done
[ "$fail" = 0 ] && echo "every mermaid diagram renders"
exit "$fail"
