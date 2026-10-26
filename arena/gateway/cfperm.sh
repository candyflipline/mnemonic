#!/bin/bash
# cfperm.sh: read-only census of what CLOUDFLARE_API_TOKEN (from the yield
# .env.1password, via op run) can do. Prints endpoint, HTTP status and names
# only; never the token, ids are cut to 8 chars.
set -euo pipefail
exec op run --env-file=/Users/user/loyal/loyal-yield-routing/.env.1password -- bash -c '
set -u
T=$(mktemp -d)
api() { curl -s -o "$T/body" -w "%{http_code}" -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" "https://api.cloudflare.com/client/v4$1"; }
c=$(api /user/tokens/verify); printf "GET /user/tokens/verify\t%s\tstatus=%s\n" "$c" "$(jq -r .result.status? "$T/body" 2>/dev/null)"
c=$(api /accounts); printf "GET /accounts\t%s\tnames=%s\n" "$c" "$(jq -r "[.result[]?.name]|join(\",\")" "$T/body")"
ACC=$(jq -r ".result[0].id // empty" "$T/body")
c=$(api /zones?per_page=50); printf "GET /zones\t%s\tnames=%s\n" "$c" "$(jq -r "[.result[]?.name]|join(\",\")" "$T/body")"
cp "$T/body" "$T/zones"
if [ -n "$ACC" ]; then
  c=$(api "/accounts/$ACC/cfd_tunnel?is_deleted=false&per_page=50"); printf "GET /accounts/{%s}/cfd_tunnel\t%s\tcount=%s errors=%s\n" "${ACC:0:8}" "$c" "$(jq -r ".result|length? // 0" "$T/body")" "$(jq -c "[.errors[]?.message]" "$T/body")"
  c=$(api "/accounts/$ACC/tokens/verify"); printf "GET /accounts/{%s}/tokens/verify\t%s\n" "${ACC:0:8}" "$c"
fi
for z in $(jq -r ".result[]?.id" "$T/zones"); do
  n=$(jq -r --arg z "$z" ".result[]|select(.id==\$z)|.name" "$T/zones")
  c=$(api "/zones/$z/dns_records?per_page=1"); printf "GET /zones/{%s}/dns_records\t%s\terrors=%s\n" "$n" "$c" "$(jq -c "[.errors[]?.message]" "$T/body")"
  c=$(api "/zones/$z/settings/grpc"); printf "GET /zones/{%s}/settings/grpc\t%s\tvalue=%s\n" "$n" "$c" "$(jq -r ".result.value? // \"-\"" "$T/body")"
  c=$(api "/zones/$z"); printf "GET /zones/{%s}\t%s\tplan=%s\n" "$n" "$c" "$(jq -r ".result.plan.name? // \"-\"" "$T/body")"
done
rm -f "$T/body" "$T/zones"; rmdir "$T"
'
