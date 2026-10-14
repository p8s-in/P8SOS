#!/bin/bash
# P8S OS: a Let's Encrypt certificate for the dashboard (certbot), renewed
# by certbot's timer. p8sd serves it for the domain and keeps its own
# certificate for everything else (the cluster pins that one).
#   acme.sh issue SPEC.json     {"domain","email","method":"http"|"cloudflare"}
#   acme.sh deploy              (certbot deploy hook: copy the files for p8sd)
#   acme.sh remove DOMAIN
#   acme.sh status
set -euo pipefail
DATA=${P8S_DATA:-/var/lib/p8s}
OUT=$DATA/tls
CF=/etc/p8s/cloudflare.ini
SELF=/usr/local/share/p8s/acme.sh

case ${1:-status} in
  issue)
    spec=${2:?spec file}
    domain=$(jq -r .domain "$spec"); email=$(jq -r '.email // ""' "$spec"); method=$(jq -r '.method // "http"' "$spec")
    [[ $domain =~ ^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$ && $domain == *.* ]] || { echo "bad domain: $domain" >&2; exit 2; }
    pkgs=certbot; [[ $method == cloudflare ]] && pkgs="certbot python3-certbot-dns-cloudflare"
    if ! command -v certbot >/dev/null || { [[ $method == cloudflare ]] && ! dpkg -s python3-certbot-dns-cloudflare >/dev/null 2>&1; }; then
      echo "→ installing $pkgs"
      DEBIAN_FRONTEND=noninteractive apt-get install -y -q $pkgs
    fi
    mail=(--register-unsafely-without-email); [[ -n $email ]] && mail=(-m "$email")
    common=(certonly -n --agree-tos "${mail[@]}" -d "$domain" --cert-name "$domain" --keep-until-expiring --deploy-hook "$SELF deploy")
    case $method in
      http)
        echo "→ asking Let's Encrypt for $domain (it checks http://$domain/ on port 80: forward TCP 80 from your router to this server)"
        certbot "${common[@]}" --standalone --preferred-challenges http ;;
      cloudflare)
        [[ -s $CF ]] || { echo "no Cloudflare API token saved" >&2; exit 2; }
        echo "→ asking Let's Encrypt for $domain (proves it through a DNS record at Cloudflare; no ports needed)"
        certbot "${common[@]}" --dns-cloudflare --dns-cloudflare-credentials "$CF" --dns-cloudflare-propagation-seconds 30 ;;
      *) echo "method must be http or cloudflare" >&2; exit 2 ;;
    esac
    # first time: certbot ran the deploy hook; with --keep-until-expiring it may not have
    RENEWED_LINEAGE=/etc/letsencrypt/live/$domain bash "$SELF" deploy
    systemctl enable --now certbot.timer >/dev/null 2>&1 || true
    echo "✔ https://$domain:8006 now has a Let's Encrypt certificate (renewed automatically)" ;;
  deploy)
    l=${RENEWED_LINEAGE:?}
    install -d -m 0700 "$OUT"
    install -m 0600 "$l/fullchain.pem" "$OUT/acme.crt.new"
    install -m 0600 "$l/privkey.pem" "$OUT/acme.key"
    mv "$OUT/acme.crt.new" "$OUT/acme.crt"   # p8sd reloads it within a minute
    echo "✔ certificate for p8sd updated" ;;
  remove)
    d=${2:-}
    [[ -n $d ]] && certbot delete -n --cert-name "$d" 2>/dev/null || true
    rm -f "$OUT/acme.crt" "$OUT/acme.key" "$CF"
    echo "✔ back to the server's own certificate" ;;
  status)
    if [[ -f $OUT/acme.crt ]]; then openssl x509 -in "$OUT/acme.crt" -noout -subject -enddate; else echo "no Let's Encrypt certificate"; fi ;;
  *) echo "usage: acme.sh issue SPEC | deploy | remove DOMAIN | status" >&2; exit 2 ;;
esac
