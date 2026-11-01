#!/bin/sh
# Forced command for the CI deploy key. In ~deploy/.ssh/authorized_keys:
#
#   command="/usr/local/bin/shiftgate-ssh",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAA… ci-deploy
#
# Install this file as /usr/local/bin/shiftgate-ssh (mode 0755). It accepts
# exactly "shiftgate deploy <service> --digest sha256:<64 hex>" and nothing else.
set -eu
cmd=${SSH_ORIGINAL_COMMAND:-}
case "$cmd" in
  *[!a-z0-9:\ -]*) echo "refused: unexpected characters" >&2; exit 64 ;;
esac
# shellcheck disable=SC2086 # word splitting is intended; characters are checked above
set -- $cmd
if [ "$#" -ne 5 ] || [ "$1" != shiftgate ] || [ "$2" != deploy ] || [ "$4" != --digest ]; then
  echo "refused: only 'shiftgate deploy <service> --digest <digest>'" >&2
  exit 64
fi
case "$3" in [a-z]*) ;; *) echo "refused: service" >&2; exit 64 ;; esac
digest=$5
case "$digest" in sha256:*) ;; *) echo "refused: digest" >&2; exit 64 ;; esac
hex=${digest#sha256:}
if [ "${#hex}" -ne 64 ]; then echo "refused: digest" >&2; exit 64; fi
exec /usr/local/bin/shiftgate deploy "$3" --digest "$digest"
