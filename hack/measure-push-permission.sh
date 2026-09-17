#!/usr/bin/env bash
# Measure what GET /repos answers for permissions.push under an installation
# token, next to the permissions GitHub reports for the token itself.
#
# -p narrows the mint to a subset of the installation's permissions, which is
# how the without-contents state is measured: the installation keeps every
# permission it holds, and only the minted token is narrowed. Record both
# answers in docs/github-app.md.
#
# It prints the App ID, the repository, the token's scope and the permission
# block. It never prints the private key or the minted token.
set -euo pipefail

usage() {
    cat >&2 <<'USAGE'
usage: measure-push-permission.sh -k <private-key.pem> -a <app-id> -i <installation-id> -r <owner/repo> [-p <permissions>]

  -k  path of the App's PEM private key
  -a  numeric App ID (4950078 for GiantSwarm Marge)
  -i  numeric installation ID (161842404 for the giantswarm organization)
  -r  the repository to probe, as owner/repo
  -p  a JSON object narrowing the minted token to a subset of the
      installation's permissions, for example '{"pull_requests":"write"}'.
      Without it the token carries everything the installation holds.
USAGE
    exit 2
}

key_path=""
app_id=""
installation_id=""
repository=""
permissions=""

while getopts "k:a:i:r:p:h" opt; do
    case "${opt}" in
        k) key_path="${OPTARG}" ;;
        a) app_id="${OPTARG}" ;;
        i) installation_id="${OPTARG}" ;;
        r) repository="${OPTARG}" ;;
        p) permissions="${OPTARG}" ;;
        *) usage ;;
    esac
done

[[ -n "${key_path}" && -n "${app_id}" && -n "${installation_id}" && -n "${repository}" ]] || usage
[[ -r "${key_path}" ]] || { echo "cannot read ${key_path}" >&2; exit 1; }

owner="${repository%%/*}"
name="${repository##*/}"
[[ "${owner}" != "${repository}" && -n "${name}" ]] || { echo "-r takes owner/repo" >&2; usage; }

base64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

now="$(date +%s)"
header="$(printf '{"alg":"RS256","typ":"JWT"}' | base64url)"
claims="$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 30))" "$((now + 540))" "${app_id}" | base64url)"
signature="$(printf '%s.%s' "${header}" "${claims}" \
    | openssl dgst -sha256 -sign "${key_path}" -binary \
    | base64url)"
app_jwt="${header}.${claims}.${signature}"

mint_body="$(jq -nc --arg name "${name}" --argjson perms "${permissions:-null}" \
    'if $perms == null then {repositories: [$name]} else {repositories: [$name], permissions: $perms} end')"

mint="$(curl -sS -X POST \
    -H "Authorization: Bearer ${app_jwt}" \
    -H "Accept: application/vnd.github+json" \
    -H "Content-Type: application/json" \
    -d "${mint_body}" \
    "https://api.github.com/app/installations/${installation_id}/access_tokens")"

token="$(printf '%s' "${mint}" | jq -r '.token // empty')"
if [[ -z "${token}" ]]; then
    echo "the mint returned no token:" >&2
    printf '%s\n' "${mint}" | jq 'del(.token)' >&2
    exit 1
fi

echo "App ${app_id}, installation ${installation_id}, repository ${repository}"
echo "requested token permissions: ${permissions:-the whole installation}"
echo "token expires at $(printf '%s' "${mint}" | jq -r '.expires_at')"
echo
echo "the token's permissions, as GitHub reports them on the mint:"
printf '%s\n' "${mint}" | jq '.permissions'
echo
echo "the token's repository scope:"
curl -sS -H "Authorization: Bearer ${token}" -H "Accept: application/vnd.github+json" \
    "https://api.github.com/installation/repositories" \
    | jq -r '.repositories[].full_name'
echo
echo "GET /repos/${repository} -> .permissions, which describes the authenticated user:"
curl -sS -H "Authorization: Bearer ${token}" -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${repository}" \
    | jq '{permissions: .permissions, push_present: (.permissions | has("push"))}'
