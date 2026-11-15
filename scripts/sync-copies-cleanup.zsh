# Source before a temporary-folder session against a persistent gateway.
# The caller supplies sync_admin, which runs the gateway's local admin CLI.
SYNC_COPIES_DEVICE=""
SYNC_COPIES_BEFORE=""
SYNC_COPIES_ID=""
sync_copies_begin() {
  local before id
  # The caller passes the physical cwd used by its session, without a trailing slash.
  id=$(print -rn -- "$2" | shasum -a 256) || return 1
  id=${id[1,16]}
  before=$(sync_admin sync list --device "$1") || return 1
  SYNC_COPIES_ID=$id
  SYNC_COPIES_BEFORE=$before
  SYNC_COPIES_DEVICE=$1
}
sync_copies_cleanup() {
  [[ -n $SYNC_COPIES_DEVICE ]] || return 0
  local after id attempt failed
  local -a before
  before=("${(@f)SYNC_COPIES_BEFORE}")
  # Unregistration can arrive just after the client exits. Remove only our cwd copies.
  for attempt in {1..20}; do
    failed=0
    if after=$(sync_admin sync list --device "$SYNC_COPIES_DEVICE"); then
      for id in ${(f)after}; do
        [[ $id == "$SYNC_COPIES_ID" || $id == "$SYNC_COPIES_ID-ro" ]] || continue
        [[ ${before[(Ie)$id]} -gt 0 ]] && continue
        sync_admin sync remove "$id" --device "$SYNC_COPIES_DEVICE" || failed=1
      done
      if (( ! failed )); then
        SYNC_COPIES_DEVICE=""
        return 0
      fi
    fi
    sleep 0.2
  done
  print -u2 "FAIL: gateway synced-copy cleanup failed for device $SYNC_COPIES_DEVICE"
  return 1
}
