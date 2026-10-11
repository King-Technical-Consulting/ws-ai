#!/usr/bin/env bash
# Checks that ws-deploy.sh deploys only from master, without a real Docker or
# Compose: a throwaway git repo holds a copy of the script, and stubs for
# `docker`, `make` and `flock`'s caller record what the script would have run.
#
#   bash infra/compose/ws-deploy.test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/ws-deploy.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin" "$work/repo/infra/compose"
cp "$script" "$work/repo/infra/compose/ws-deploy.sh"
log="$work/calls.log"
: >"$log"
for tool in docker make; do
  printf '#!/usr/bin/env bash\necho "%s $*" >>"%s"\nexit 0\n' "$tool" "$log" >"$work/bin/$tool"
  chmod +x "$work/bin/$tool"
done

cd "$work/repo"
git init -q -b master .
git -c user.name=t -c user.email=t@t commit -q --allow-empty -m base
export PATH="$work/bin:$PATH" XDG_RUNTIME_DIR="$work" HOME="$work"

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { bash infra/compose/ws-deploy.sh >"$work/out.txt" 2>&1; echo $?; }

# 1. on master it goes on to pull and apply
code=$(run || true)
[ "$code" = 0 ] || fail "on master: exit $code: $(cat "$work/out.txt")"
grep -q '^make -s up' "$log" || fail "on master: it did not apply the stack ($(cat "$log"))"

# 2. on a feature branch it refuses, names the branch, and touches nothing
: >"$log"
git checkout -q -b feature/wip
code=$(run || true)
[ "$code" = 1 ] || fail "on a branch: exit $code, want 1"
grep -q "refusing to deploy: this checkout is on 'feature/wip'" "$work/out.txt" || fail "on a branch: the refusal does not name it: $(cat "$work/out.txt")"
[ ! -s "$log" ] || fail "on a branch: the stack was touched: $(cat "$log")"

# 3. a detached HEAD is refused too
: >"$log"
git checkout -q --detach
code=$(run || true)
[ "$code" = 1 ] || fail "detached: exit $code, want 1"
grep -q "'HEAD'" "$work/out.txt" || fail "detached: the refusal does not say so: $(cat "$work/out.txt")"
[ ! -s "$log" ] || fail "detached: the stack was touched: $(cat "$log")"

# 4. back on master it deploys again
git checkout -q master
: >"$log"
code=$(run || true)
[ "$code" = 0 ] || fail "back on master: exit $code"
grep -q '^make -s up' "$log" || fail "back on master: it did not apply the stack"

echo "ok: deploys from master, refuses a branch and a detached HEAD, touches nothing when it refuses"
