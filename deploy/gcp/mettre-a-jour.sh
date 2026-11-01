#!/bin/bash
# Mise a jour de l'instance VM depuis les versions publiees par GitHub (voir .github/workflows/ci.yml
# des depots mytv et mytv-ui). La VM TIRE : rien n'entre par SSH depuis GitHub.
#
# Usage (en root) : mettre-a-jour.sh [serveur|interface|tout] [etiquette]   (defaut : tout, derniere version)
#                   mettre-a-jour.sh retour                                   (revient a la version precedente)
#                   mettre-a-jour.sh etat
# Prerequis : /etc/mytv/github.token (600 root) = jeton GitHub en LECTURE SEULE (Contents: read) sur les
# deux depots prives ; curl, jq, sha256sum.
set -euo pipefail
PROPRIETAIRE=${PROPRIETAIRE:-msounkere}
JETON_FICHIER=/etc/mytv/github.token
BIN=/opt/mytv/bin/mytv
COMPTES=/opt/mytv/bin/mytv-comptes
DIST=/opt/mytv-ui/dist
VERSIONS=/var/lib/mytv/versions   # journal des versions installees
TRAVAIL=$(mktemp -d /tmp/mytv-maj.XXXXXX)
trap 'rm -rf "$TRAVAIL"' EXIT

jeton() { [ -r "$JETON_FICHIER" ] || { echo "manque $JETON_FICHIER" >&2; exit 1; }; tr -d '[:space:]' < "$JETON_FICHIER"; }
api() { curl -sfL -m 60 -H "Authorization: Bearer $(jeton)" -H "Accept: application/vnd.github+json" "$@"; }

# version <depot> [etiquette] -> JSON de la version (derniere par defaut)
version() {
  if [ -n "${2:-}" ]; then api "https://api.github.com/repos/$PROPRIETAIRE/$1/releases/tags/$2"
  else api "https://api.github.com/repos/$PROPRIETAIRE/$1/releases/latest"; fi
}

# telecharger <json version> <nom d'actif> <destination> : actif + verification SHA256SUMS
telecharger() {
  local json=$1 nom=$2 dest=$3 id somme
  for actif in "$nom" SHA256SUMS; do
    id=$(jq -r --arg n "$actif" '.assets[] | select(.name==$n) | .id' <<<"$json")
    [ -n "$id" ] || { echo "actif $actif absent de la version" >&2; exit 1; }
    curl -sfL -m 600 -H "Authorization: Bearer $(jeton)" -H "Accept: application/octet-stream" \
      -o "$TRAVAIL/$actif" "https://api.github.com/repos/$PROPRIETAIRE/$4/releases/assets/$id"
  done
  somme=$(grep " $nom\$" "$TRAVAIL/SHA256SUMS" | cut -d' ' -f1)
  echo "$somme  $TRAVAIL/$nom" | sha256sum -c --quiet || { echo "somme de controle invalide pour $nom" >&2; exit 1; }
  mv "$TRAVAIL/$nom" "$dest"
}

noter() { mkdir -p "$(dirname $VERSIONS)"; echo "$(date -u +%FT%TZ) $1 $2" >> "$VERSIONS"; }

serveur() {
  local json etiquette
  json=$(version mytv "${1:-}"); etiquette=$(jq -r .tag_name <<<"$json")
  if [ -f "$BIN.version" ] && [ "$(cat "$BIN.version")" = "$etiquette" ]; then echo "serveur deja en $etiquette"; return; fi
  echo "serveur : $etiquette"
  telecharger "$json" mytv-linux-amd64 "$TRAVAIL/mytv" mytv
  telecharger "$json" mytv-comptes-linux-amd64 "$TRAVAIL/mytv-comptes" mytv
  chmod 755 "$TRAVAIL/mytv" "$TRAVAIL/mytv-comptes"
  # La configuration lit ses secrets dans l'environnement : meme environnement que l'unite.
  ( set -a; . /etc/mytv/mytv.env; set +a; "$TRAVAIL/mytv" --config /etc/mytv/mytv.yaml -verifier-config >/dev/null )
  [ -f "$BIN" ] && cp -p "$BIN" "$BIN.precedent" && cp -p "$BIN.version" "$BIN.version.precedent" 2>/dev/null || true
  install -m 755 "$TRAVAIL/mytv" "$BIN"; install -m 755 "$TRAVAIL/mytv-comptes" "$COMPTES"
  echo "$etiquette" > "$BIN.version"
  systemctl restart mytv
  sleep 3; systemctl is-active --quiet mytv || { echo "mytv ne demarre pas : retour arriere" >&2; retour_serveur; exit 1; }
  noter serveur "$etiquette"
}

interface() {
  local json etiquette
  json=$(version mytv-ui "${1:-}"); etiquette=$(jq -r .tag_name <<<"$json")
  if [ -f "$DIST.version" ] && [ "$(cat "$DIST.version")" = "$etiquette" ]; then echo "interface deja en $etiquette"; return; fi
  echo "interface : $etiquette"
  telecharger "$json" mytv-ui-dist.tgz "$TRAVAIL/dist.tgz" mytv-ui
  mkdir -p "$TRAVAIL/dist" && tar xzf "$TRAVAIL/dist.tgz" -C "$TRAVAIL/dist" --strip-components=1
  [ -f "$TRAVAIL/dist/index.html" ] || { echo "archive sans index.html" >&2; exit 1; }
  rm -rf "$DIST.precedent"; [ -d "$DIST" ] && mv "$DIST" "$DIST.precedent" && mv "$DIST.version" "$DIST.version.precedent" 2>/dev/null || true
  mv "$TRAVAIL/dist" "$DIST"; chmod -R a+rX "$DIST"
  echo "$etiquette" > "$DIST.version"
  noter interface "$etiquette"
}

retour_serveur() {
  [ -f "$BIN.precedent" ] || { echo "pas de version precedente du serveur" >&2; return 1; }
  cp -p "$BIN.precedent" "$BIN"; cp -p "$BIN.version.precedent" "$BIN.version" 2>/dev/null || true
  systemctl restart mytv; noter serveur "retour:$(cat "$BIN.version" 2>/dev/null)"
}
retour_interface() {
  [ -d "$DIST.precedent" ] || { echo "pas de version precedente de l'interface" >&2; return 1; }
  rm -rf "$DIST"; mv "$DIST.precedent" "$DIST"; mv "$DIST.version.precedent" "$DIST.version" 2>/dev/null || true
  noter interface "retour:$(cat "$DIST.version" 2>/dev/null)"
}

etat() {
  echo "serveur   : $(cat "$BIN.version" 2>/dev/null || echo inconnue) (precedente : $(cat "$BIN.version.precedent" 2>/dev/null || echo -))"
  echo "interface : $(cat "$DIST.version" 2>/dev/null || echo inconnue) (precedente : $(cat "$DIST.version.precedent" 2>/dev/null || echo -))"
  echo "disponibles : serveur $(version mytv | jq -r .tag_name), interface $(version mytv-ui | jq -r .tag_name)"
  systemctl is-active mytv
}

case "${1:-tout}" in
  serveur) serveur "${2:-}";;
  interface) interface "${2:-}";;
  tout) serveur "${2:-}"; interface "${2:-}";;
  retour) retour_serveur; retour_interface;;
  etat) etat;;
  *) echo "usage : $0 [serveur|interface|tout] [etiquette] | retour | etat" >&2; exit 2;;
esac
