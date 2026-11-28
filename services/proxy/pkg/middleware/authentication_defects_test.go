//go:build defects

package middleware

// Pruebas que expresan el comportamiento ESPERADO según el contrato y que
// hoy FALLAN: evidencia de los defectos D1 y D2 del componente #4.
// Están detrás del build tag "defects" para no romper la suite normal.
//
// Ejecutar:  go test -tags defects ./services/proxy/pkg/middleware/ -run 'TestDefectAuth' -v

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// D1: CredentialsByUserAgent "sets the auth challenges on a per user-agent
// basis". Una petición SIN User-Agent no corresponde a ningún user agent
// configurado, así que debería recibir todos los challenges, siempre igual.
// Hoy strings.Contains(clave, "") es true para cualquier clave: se fija el
// challenge de la primera entrada del map, cuyo orden es aleatorio en Go.
func TestDefectAuthD1EmptyUserAgentLocksRandomChallenge(t *testing.T) {
	locks := CredentialsByUserAgent(map[string]string{"mirall": "basic", "ios-app": "bearer"})
	t.Run("CH-19 sin User-Agent: todos los challenges, determinista", func(t *testing.T) {
		seen := map[string]int{}
		for i := 0; i < 100; i++ {
			r := newReq(http.MethodGet, "/graph/v1.0/me", false)
			r.Header.Del("User-Agent")
			o := serve(t, []Authenticator{fake("a", NotApplicable())}, r, OIDCIss("x"), EnableBasicAuth(true), locks)
			got := o.challenges()
			key := ""
			for _, c := range got {
				key += c + " | "
			}
			seen[key]++
		}
		want := bearerCh + " | " + basicCh + " | "
		if len(seen) != 1 || seen[want] != 100 {
			t.Errorf("respuestas distintas en 100 ejecuciones = %v; esperado siempre %q", seen, want)
		}
	})
}

// D2: el código intenta responder 500 cuando no puede renderizar un fallo
// terminal (authentication.go:189-192), pero el 401 ya se escribió dentro
// de renderJSONExpired / renderDAVExpired; el WriteHeader(500) se ignora.
func TestDefectAuthD2RenderFailureReturns500(t *testing.T) {
	t.Run("TF-07 falla el render: el cliente debería recibir 500", func(t *testing.T) {
		res := AuthenticationResult{State: AuthenticationFailed, Terminal: true, ErrorDetails: GuestSessionExpiredDetails{PermissionID: "p"}}
		resetStrategies(t)
		h := Authentication([]Authenticator{fake("a", res)})(http.NotFoundHandler())
		w := failingWriter{httptest.NewRecorder()}
		h.ServeHTTP(w, newReq(http.MethodGet, "/s/abc", false))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("esperado 500, observado %d", w.Code)
		}
	})
}
