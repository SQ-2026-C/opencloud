//go:build defects

package router

// Pruebas que expresan el comportamiento ESPERADO según el contrato y que
// hoy FALLAN: evidencia de los defectos D1 y D2 del componente #5.
// Están detrás del build tag "defects" para no romper la suite normal.
//
// Ejecutar:  go test -tags defects ./services/proxy/pkg/router/ -run 'Defect' -v

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opencloud-eu/opencloud/services/proxy/pkg/config"
)

// routeNoPanic convierte un pánico de Route en un error del test.
func routeNoPanic(rt Router, r *http.Request) (ri RoutingInfo, ok bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("pánico: %v", p)
		}
	}()
	ri, ok = rt.Route(r)
	return
}

// D1: "Method optionally limits the route to this HTTP method" (config.go).
// Agregar una ruta prefix limitada a GET no debería cambiar a dónde van las
// demás peticiones GET; hoy oculta todas las rutas prefix genéricas.
func TestDefectD1MethodRouteShadowsGenericRoutes(t *testing.T) {
	rt := newRouter(nil,
		config.Route{Endpoint: "/", Backend: "http://web"},
		config.Route{Endpoint: "/graph/", Backend: "http://graph"},
		config.Route{Endpoint: "/dav/", Method: http.MethodGet, Backend: "http://dav"},
	)
	t.Run("RT-15 GET a ruta genérica con otra ruta GET presente", func(t *testing.T) {
		ri, ok, err := routeNoPanic(rt, httptest.NewRequest(http.MethodGet, "/graph/v1.0/me", nil))
		if err != nil || !ok || ri.endpoint != "/graph/" {
			t.Errorf("esperado /graph/, observado endpoint=%q ok=%v err=%v", ri.endpoint, ok, err)
		}
	})
	t.Run("RT-15b la ruta GET sigue funcionando", func(t *testing.T) {
		ri, ok, err := routeNoPanic(rt, httptest.NewRequest(http.MethodGet, "/dav/f", nil))
		if err != nil || !ok || ri.endpoint != "/dav/" {
			t.Errorf("esperado /dav/, observado endpoint=%q ok=%v err=%v", ri.endpoint, ok, err)
		}
	})
}

// D2: si no hay coincidencia, el contrato (y el Middleware) esperan
// (noInfo, false) para responder 500. Hoy el respaldo indexa [0] sobre una
// lista vacía y entra en pánico.
func TestDefectD2FallbackPanics(t *testing.T) {
	cases := []struct {
		id     string
		routes []config.Route
		method string
	}{
		{"RT-16 solo rutas prefix con método, petición de otro método", []config.Route{{Endpoint: "/a", Method: http.MethodGet, Backend: "http://a"}}, http.MethodPost},
		{"RT-17 política sin rutas prefix (solo regex)", []config.Route{{Type: config.RegexRoute, Endpoint: "^/a", Backend: "http://a"}}, http.MethodGet},
		{"RT-18 ruta con método que no coincide y sin rutas genéricas", []config.Route{{Endpoint: "/a", Method: http.MethodGet, Backend: "http://a"}}, http.MethodGet},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			ri, ok, err := routeNoPanic(newRouter(nil, c.routes...), httptest.NewRequest(c.method, "/b", nil))
			if err != nil || ok || ri.endpoint != "" {
				t.Errorf("esperado (noInfo, false) sin pánico, observado endpoint=%q ok=%v err=%v", ri.endpoint, ok, err)
			}
		})
	}
}
