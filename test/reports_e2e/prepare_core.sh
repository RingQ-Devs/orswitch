#!/bin/bash
# A private, patched copy of RingQ Core's PHP app that talks to the sandbox (sandbox.sh) instead of the live
# PostgreSQL/Redis. Used to run Core's own call-record import (MainTask calldetailrecord) and its report classes
# on the records a test produced. The live /var/www/ringq is only read.
BASE=${OR_SBX:-/tmp/rq_reports_sbx}; export OR_BASE=$BASE
CORE=${OR_CORE:-/tmp/rq_reports_core}
SRC=${OR_CORE_SRC:-/var/www/ringq}
[ -d "$CORE/app" ] && [ "$1" != "-f" ] && { echo "core copy exists"; exit 0; }
rm -rf "$CORE"; mkdir -p "$CORE/cache"
chmod 700 "$CORE"   # a copy of the application code: private
cp -r "$SRC/app" "$CORE/app"
ln -s "$SRC/vendor" "$CORE/vendor"
# database: the sandbox's unix socket directory (trust authentication: the live password is not kept)
sed -i -E "s#('host'[[:space:]]*=>[[:space:]]*)'127.0.0.1'#\1'$BASE'#" "$CORE/app/config/config.php"
perl -pi -e "s/('password'\s*=>\s*)'[^']*'/\$1'sandbox'/" "$CORE/app/config/config.php"
# redis: the sandbox's socket
perl -pi -e 's|new Predis.Client\(\);|new Predis\x5cClient(["scheme" => "unix", "path" => "$ENV{OR_BASE}/redis.sock"]);|' "$CORE/app/cli.php"
grep -q "$BASE" "$CORE/app/config/config.php" && grep -q "$BASE/redis.sock" "$CORE/app/cli.php" || { echo "patching failed"; exit 1; }
# runner: cli.php's bootstrap (everything before the console is created) + the report runner
n=$(grep -n '^\$console = new ConsoleApp' "$CORE/app/cli.php" | cut -d: -f1)
head -n $((n-1)) "$CORE/app/cli.php" > "$CORE/app/runner.php"
cat "$(dirname "$0")/run_reports.php" | sed '1d' >> "$CORE/app/runner.php"
echo "core copy ready: $CORE"
