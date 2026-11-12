#!/usr/bin/env bash

# Share bounded transport retries across image download backends. Authentication,
# missing tags/platforms, rate limits and local storage failures are not retried.
eva_docker_pull_image() {
  local image="$1"
  shift
  local attempt diagnostic status delay
  local -a docker_command
  read -r -a docker_command <<< "${DOCKER_CMD:-docker}"
  for attempt in 1 2 3; do
    diagnostic="$(mktemp "${TMPDIR:-/tmp}/eva-docker-pull.XXXXXX")" || return "$?"
    if "${docker_command[@]}" pull "$@" "$image" 2>&1 | tee "$diagnostic"; then
      rm -f "$diagnostic"
      return 0
    else
      status=$?
    fi
    if (( attempt == 3 )) || grep -Eiq \
      'unauthorized:|denied:|toomanyrequests:|pull rate limit|no space left on device|manifest unknown|no matching manifest|not found' \
      "$diagnostic" || ! grep -Eiq \
      'TLS handshake timeout|i/o timeout|connection (reset by peer|refused)|unexpected EOF|(^|[^[:alpha:]])EOF([^[:alpha:]]|$)|context deadline exceeded|Client.Timeout exceeded|request canceled.*(timeout|deadline)|temporary failure in name resolution|Service Unavailable|Bad Gateway|Gateway Timeout|(HTTP|status)[^[:cntrl:]]*(502|503|504)' \
      "$diagnostic"; then
      rm -f "$diagnostic"
      echo "[ERROR] image pull failed after $attempt attempt(s): $image" >&2
      return "$status"
    fi
    rm -f "$diagnostic"
    delay=$((3 * attempt))
    echo "[retry] transient image pull failure: $image; retry $((attempt + 1))/3 in ${delay}s" >&2
    sleep "$delay" || return "$?"
  done
}
