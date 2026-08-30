#!/bin/sh
set -eu

# Hash tracked plus non-ignored untracked file CONTENTS. Unlike HEAD+diff this
# stays identical before and after committing the exact same source tree.
git -c core.quotepath=false ls-files -c -o --exclude-standard \
	| LC_ALL=C sort \
	| while IFS= read -r path; do
		[ -f "$path" ] || continue
		printf '%s\t%s\n' "$path" "$(git hash-object -- "$path")"
	done \
	| git hash-object --stdin
