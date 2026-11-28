#!/usr/bin/env bash
# Entrega 1 (Calidad de Software, equipo C): un solo comando que ejecuta
# todas las suites de verificación de componentes, una por capa, y da un
# veredicto por cada test.
#
# Uso (desde cualquier carpeta):
#   qa/run-tests.sh             # suites de contrato (deben pasar)
#   qa/run-tests.sh --defects   # además, la evidencia de defectos (falla a propósito)
#
# Para agregar una capa: escribir una función tier_<nombre> que imprima una
# línea "PASS <id>" o "FAIL <id>" por test y agregarla a TIERS.

set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
WITH_DEFECTS=0
[[ "${1:-}" == "--defects" ]] && WITH_DEFECTS=1

# Convierte la salida de `go test -v` en una línea de veredicto por (sub)test.
go_verdicts() {
  sed -nE 's/^ *--- (PASS|FAIL|SKIP): ([^ ]+).*/\1 \2/p'
}

# ---- Capa de lógica / API (Go): componentes #4 y #5 del proxy ----
tier_go() {
  go test -count=1 -v -run 'Contract' \
    ./services/proxy/pkg/router/ \
    ./services/proxy/pkg/middleware/ 2>&1 | go_verdicts
  if [[ $WITH_DEFECTS == 1 ]]; then
    go test -count=1 -v -tags defects -run 'Defect' \
      ./services/proxy/pkg/router/ \
      ./services/proxy/pkg/middleware/ 2>&1 | go_verdicts | sed 's/^/[defects] /'
  fi
}

# ---- Otras capas del equipo (datos, UI, ...): agregar aquí ----
# tier_data() { ...; }

TIERS=(go)

overall=0
for t in "${TIERS[@]}"; do
  echo "=================== Capa: $t ==================="
  out="$("tier_$t")"
  echo "$out"
  pass=$(grep -c '^PASS' <<<"$out")
  fail=$(grep -c '^FAIL' <<<"$out")
  dpass=$(grep -c '^\[defects\] PASS' <<<"$out")
  dfail=$(grep -c '^\[defects\] FAIL' <<<"$out")
  echo "--- $t: $pass PASS, $fail FAIL"
  [[ $WITH_DEFECTS == 1 ]] && echo "--- $t (defectos, se espera FAIL): $dpass PASS, $dfail FAIL"
  if [[ $fail -gt 0 || $pass -eq 0 ]]; then overall=1; fi
done
echo "=================== Veredicto: $([[ $overall == 0 ]] && echo OK || echo FALLA) ==================="
exit $overall
