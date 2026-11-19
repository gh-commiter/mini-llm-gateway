# Shared health wait for the Compose rehearsals: project, optional attempts and delay.
healthy() {
  local i st tries=${2:-40} delay=${3:-5}
  for (( i=1; i<=tries; i++ )); do
    st=$(docker inspect --format '{{.State.Health.Status}}' $1-minigw-1 2>/dev/null) || st=missing
    [[ $st == healthy ]] && return 0
    sleep $delay
  done
  return 1
}
