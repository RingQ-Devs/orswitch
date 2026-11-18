#!/bin/bash
# Private, throw-away copy of the RingQ database and a private Redis, for the reports end-to-end test.
#   sandbox.sh up      copy the live database (read-only dump) into a private PostgreSQL + start a private Redis
#   sandbox.sh down    stop and remove both
# Nothing here touches the live PostgreSQL/Redis other than one read-only pg_dump.
# Private PostgreSQL: unix socket in $BASE (port 5432, no TCP), trust auth. Private Redis: $BASE/redis.sock.
BASE=${OR_SBX:-/tmp/rq_reports_sbx}
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | tail -1)
case "$1" in
up)
  [ -S "$BASE/.s.PGSQL.5432" ] && { echo "already up"; exit 0; }
  rm -rf "$BASE"; mkdir -p "$BASE"; chmod 755 "$BASE"; chown postgres:postgres "$BASE"
  su postgres -s /bin/sh -c "cd /tmp && $PGBIN/initdb -D $BASE/pg -A trust -U postgres >/dev/null" || exit 1
  su postgres -s /bin/sh -c "cd /tmp && $PGBIN/pg_ctl -D $BASE/pg -o \"-c listen_addresses='' -c unix_socket_directories=$BASE -c port=5432 -c fsync=off -c synchronous_commit=off\" -l $BASE/pg.log -w start >/dev/null" || exit 1
  su postgres -s /bin/sh -c "cd /tmp && $PGBIN/psql -h $BASE -U postgres -qAtc \"create role ringq superuser login\" -c \"create database ringq owner ringq\"" || exit 1
  # read-only dump of the live database, restored into the private one
  su postgres -s /bin/sh -c "cd /tmp && env -u PGUSER $PGBIN/pg_dump -d ringq -Fc --no-owner --no-acl -f $BASE/ringq.dump" || exit 1
  su postgres -s /bin/sh -c "cd /tmp && $PGBIN/pg_restore -h $BASE -U postgres -d ringq --no-owner --no-acl $BASE/ringq.dump >$BASE/restore.log 2>&1"
  rm -f "$BASE/ringq.dump"
  redis-server --port 0 --unixsocket "$BASE/redis.sock" --unixsocketperm 777 --save "" --appendonly no --daemonize yes --dir "$BASE" --pidfile "$BASE/redis.pid" --logfile "$BASE/redis.log" >/dev/null
  sleep 1; [ -S "$BASE/redis.sock" ] && echo "sandbox up: $BASE" || { echo "redis did not start"; exit 1; }
  ;;
down)
  [ -f "$BASE/redis.pid" ] && kill "$(cat $BASE/redis.pid)" 2>/dev/null
  [ -d "$BASE/pg" ] && su postgres -s /bin/sh -c "cd /tmp && $PGBIN/pg_ctl -D $BASE/pg -m immediate stop >/dev/null 2>&1"
  rm -rf "$BASE"; echo "sandbox removed"
  ;;
*) echo "usage: sandbox.sh up|down"; exit 2;;
esac
