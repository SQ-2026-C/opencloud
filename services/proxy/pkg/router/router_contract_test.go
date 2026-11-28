package router

// Pruebas de verificación a nivel de componente del Router del proxy
// (SQ 2026-II, Entrega 1, equipo C, componente #5).
//
// Cada subtest lleva el ID del caso de prueba documentado en
// p1/componente-5-router.md. Convención de IDs:
//   PM = prefixRouteMatcher, QM = queryRouteMatcher, RM = regexRouteMatcher,
//   SJ = singleJoiningSlash, RT = Route (tabla de decisión),
//   DC = Route con la configuración por defecto, RW = rewrite hook,
//   MW = Middleware, NW = New (comportamiento excepcional).
//
// Ejecutar:  go test ./services/proxy/pkg/router/ -run 'Contract' -v

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/proxy/pkg/config"
	"github.com/opencloud-eu/opencloud/services/proxy/pkg/config/defaults"
	"github.com/rs/zerolog"
	"go-micro.dev/v4/registry"
	"go-micro.dev/v4/selector"
)

// ---------------------------------------------------------------------------
// Dobles de prueba
// ---------------------------------------------------------------------------

// stubServiceSelector reemplaza al selector de go-micro (que consulta el
// registro y elige un nodo al azar). Es un stub (devuelve un nodo o un error
// fijo) y a la vez un spy (registra qué servicios se pidieron).
type stubServiceSelector struct {
	node      *registry.Node
	selectErr error
	nextErr   error
	asked     []string
}

func (s *stubServiceSelector) Init(...selector.Option) error { return nil }
func (s *stubServiceSelector) Options() selector.Options     { return selector.Options{} }
func (s *stubServiceSelector) Select(service string, _ ...selector.SelectOption) (selector.Next, error) {
	s.asked = append(s.asked, service)
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	return func() (*registry.Node, error) {
		if s.nextErr != nil {
			return nil, s.nextErr
		}
		return s.node, nil
	}, nil
}
func (s *stubServiceSelector) Mark(string, *registry.Node, error) {}
func (s *stubServiceSelector) Reset(string)                       {}
func (s *stubServiceSelector) Close() error                       { return nil }
func (s *stubServiceSelector) String() string                     { return "stub" }

// staticPolicy es la configuración del selector de políticas usada en casi
// todos los casos: siempre devuelve la política "p" (determinista).
var staticPolicy = &config.PolicySelector{Static: &config.StaticSelectorConf{Policy: "p"}}

// newRouter construye el router con una única política "p", logger dummy y
// el selector de servicios indicado (puede ser nil si no se usa rewrite).
func newRouter(sel selector.Selector, routes ...config.Route) Router {
	return New(sel, staticPolicy, []config.Policy{{Name: "p", Routes: routes}}, log.NopLogger())
}

// spyLogger devuelve un logger real que escribe en un buffer (spy).
// El nivel global de zerolog lo modifica el init de pkg/log (go-micro), así
// que se fija explícitamente y se restaura al final del test.
func spyLogger(t *testing.T) (log.Logger, *bytes.Buffer) {
	t.Helper()
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	var buf bytes.Buffer
	return log.Logger{Logger: zerolog.New(&buf)}, &buf
}

func mustURL(t *testing.T, raw string) url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url inválida en el caso de prueba %q: %v", raw, err)
	}
	return *u
}

// ---------------------------------------------------------------------------
// prefixRouteMatcher — partición de equivalencia y valores límite
// ---------------------------------------------------------------------------

func TestContractPrefixRouteMatcher(t *testing.T) {
	cases := []struct {
		id, prefix, target string
		want               bool
	}{
		{"PM-01 ruta con el prefijo (clase válida)", "/graph/", "/graph/v1.0/me", true},
		{"PM-02 ruta sin el prefijo (clase inválida)", "/graph/", "/ocs/v1.php", false},
		{"PM-03 límite: ruta igual al prefijo", "/graph/", "/graph/", true},
		{"PM-04 límite: ruta un carácter más corta (sin / final)", "/graph/", "/graph", false},
		{"PM-05 prefijo raíz nunca coincide", "/", "/cualquier/cosa", false},
		{"PM-06 límite: prefijo raíz contra la raíz", "/", "/", false},
		{"PM-07 '..' se normaliza antes de comparar", "/dav/", "/x/../dav/f", true},
		{"PM-08 '//' se normaliza antes de comparar", "/dav/", "//dav//f", true},
		{"PM-09 '/' final se conserva tras normalizar", "/dav/", "/dav/./", true},
		{"PM-10 ruta vacía se vuelve '.' y no coincide", "/konnect/", "", false},
		{"PM-11 coincidencia por texto, no por segmento", "/status", "/statusXYZ", true},
		{"PM-12 sensible a mayúsculas", "/graph/", "/GRAPH/v1.0/me", false},
		{"PM-13 '..' no permite escapar de una ruta protegida a una sin protección", "/konnect/", "/konnect/../graph/v1.0/users", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			// Se construye la URL con Path directo: url.Parse("//dav//f") tomaría "dav" como host.
			if got := prefixRouteMatcher(c.prefix, url.URL{Path: c.target}); got != c.want {
				t.Errorf("prefixRouteMatcher(%q, %q) = %v, esperado %v", c.prefix, c.target, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// queryRouteMatcher — partición de equivalencia y valores límite
// ---------------------------------------------------------------------------

func TestContractQueryRouteMatcher(t *testing.T) {
	cases := []struct {
		id, endpoint, target string
		want                 bool
	}{
		{"QM-01 prefijo y parámetro iguales", "/dav?preview=1", "/dav/files/a.png?preview=1", true},
		{"QM-02 valor del parámetro distinto", "/dav?preview=1", "/dav/files/a.png?preview=0", false},
		{"QM-03 endpoint sin query nunca coincide", "/dav", "/dav/files/a.png?preview=1", false},
		{"QM-04 endpoint raíz nunca coincide", "/", "/?preview=1", false},
		{"QM-05 ruta sin el prefijo", "/dav?preview=1", "/graph?preview=1", false},
		{"QM-06 todos los parámetros del endpoint presentes", "/x?a=1&b=2", "/x/y?b=2&a=1", true},
		{"QM-07 falta uno de los parámetros", "/x?a=1&b=2", "/x/y?a=1", false},
		{"QM-08 parámetros extra en la petición se ignoran", "/x?a=1", "/x/y?a=1&z=9", true},
		{"QM-09 endpoint con '/' final no coincide consigo mismo", "/remote.php/?preview=1", "/remote.php/?preview=1", false},
		{"QM-10 parámetro vacío equivale a ausente", "/x/?a=", "/x/y", true},
		{"QM-11 parámetro repetido: solo cuenta el primero", "/x?preview=1", "/x/y?preview=0&preview=1", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := queryRouteMatcher(c.endpoint, mustURL(t, c.target)); got != c.want {
				t.Errorf("queryRouteMatcher(%q, %q) = %v, esperado %v", c.endpoint, c.target, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// regexRouteMatcher
// ---------------------------------------------------------------------------

func TestContractRegexRouteMatcher(t *testing.T) {
	cases := []struct {
		id, pattern, target string
		want                bool
	}{
		{"RM-01 coincide con la regex REPORT por defecto", "(/remote.php)?/(web)?dav", "/remote.php/dav/files/admin", true},
		{"RM-02 no coincide", "(/remote.php)?/(web)?dav", "/graph/v1.0/me", false},
		{"RM-04 regex no anclada: coincide dentro de la query", "/ocs/v[12].php/config", "/graph/v1.0/users?q=/ocs/v1.php/config", true},
		{"RM-05 '.' sin escapar coincide con cualquier carácter", "/ocs/v[12].php/config", "/ocs/v1Xphp/config", true},
	}
	rt := Router{logger: log.NopLogger()}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := rt.regexRouteMatcher(c.pattern, mustURL(t, c.target)); got != c.want {
				t.Errorf("regexRouteMatcher(%q, %q) = %v, esperado %v", c.pattern, c.target, got, c.want)
			}
		})
	}

	t.Run("RM-03 regex inválida: no coincide y deja advertencia", func(t *testing.T) {
		l, buf := spyLogger(t)
		rt := Router{logger: l}
		if rt.regexRouteMatcher("(", mustURL(t, "/(")) {
			t.Errorf("una regex inválida no debe coincidir")
		}
		if !strings.Contains(buf.String(), "regex with pattern failed") {
			t.Errorf("se esperaba una advertencia en el log, se obtuvo %q", buf.String())
		}
	})
}

// ---------------------------------------------------------------------------
// singleJoiningSlash — valores límite de la '/' de unión
// ---------------------------------------------------------------------------

func TestContractSingleJoiningSlash(t *testing.T) {
	cases := []struct{ id, a, b, want string }{
		{"SJ-01 ambos con '/'", "/base/", "/x", "/base/x"},
		{"SJ-02 ninguno con '/'", "/base", "x", "/base/x"},
		{"SJ-03 solo el primero", "/base/", "x", "/base/x"},
		{"SJ-04 solo el segundo", "/base", "/x", "/base/x"},
		{"SJ-05 base vacía", "", "/x", "/x"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := singleJoiningSlash(c.a, c.b); got != c.want {
				t.Errorf("singleJoiningSlash(%q, %q) = %q, esperado %q", c.a, c.b, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Route — tabla de decisión
// ---------------------------------------------------------------------------

func TestContractRouteDecisionTable(t *testing.T) {
	root := config.Route{Endpoint: "/", Backend: "http://web"}

	type expect struct {
		ok       bool
		endpoint string
	}
	cases := []struct {
		id     string
		routes []config.Route
		method string
		target string
		want   expect
	}{
		{
			id:     "RT-03 query tiene precedencia sobre prefix",
			routes: []config.Route{root, {Endpoint: "/dav/", Backend: "http://frontend"}, {Type: config.QueryRoute, Endpoint: "/dav?preview=1", Backend: "http://thumbs"}},
			method: http.MethodGet, target: "/dav/a.png?preview=1",
			want: expect{true, "/dav?preview=1"},
		},
		{
			id:     "RT-04 regex tiene precedencia sobre prefix",
			routes: []config.Route{root, {Endpoint: "/ocs/", Backend: "http://frontend"}, {Type: config.RegexRoute, Endpoint: "^/ocs/v[12]\\.php/config", Backend: "http://cfg"}},
			method: http.MethodGet, target: "/ocs/v1.php/config",
			want: expect{true, "^/ocs/v[12]\\.php/config"},
		},
		{
			id:     "RT-05 query tiene precedencia sobre regex",
			routes: []config.Route{root, {Type: config.RegexRoute, Endpoint: "/dav", Backend: "http://re"}, {Type: config.QueryRoute, Endpoint: "/dav?preview=1", Backend: "http://q"}},
			method: http.MethodGet, target: "/dav/a?preview=1",
			want: expect{true, "/dav?preview=1"},
		},
		{
			id:     "RT-06 coincidencia prefix genérica",
			routes: []config.Route{root, {Endpoint: "/graph/", Backend: "http://graph"}},
			method: http.MethodGet, target: "/graph/v1.0/me",
			want: expect{true, "/graph/"},
		},
		{
			id:     "RT-07 a igual tipo gana la primera declarada, aunque sea menos específica",
			routes: []config.Route{root, {Endpoint: "/graph/", Backend: "http://graph"}, {Endpoint: "/graph/v1.0/invitations", Backend: "http://inv"}},
			method: http.MethodGet, target: "/graph/v1.0/invitations",
			want: expect{true, "/graph/"},
		},
		{
			id:     "RT-08 ruta con método coincide para ese método",
			routes: []config.Route{root, {Endpoint: "/dav/", Backend: "http://frontend"}, {Type: config.RegexRoute, Method: "REPORT", Endpoint: "/dav", Backend: "http://webdav"}},
			method: "REPORT", target: "/dav/files",
			want: expect{true, "/dav"},
		},
		{
			id:     "RT-09 ruta con método no aplica a otro método",
			routes: []config.Route{root, {Endpoint: "/dav/", Backend: "http://frontend"}, {Type: config.RegexRoute, Method: "REPORT", Endpoint: "/dav", Backend: "http://webdav"}},
			method: "PROPFIND", target: "/dav/files",
			want: expect{true, "/dav/"},
		},
		{
			id:     "RT-10 sin coincidencia y '/' es la primera ruta: respaldo a '/'",
			routes: []config.Route{root, {Endpoint: "/graph/", Backend: "http://graph"}},
			method: http.MethodGet, target: "/index.html",
			want: expect{true, "/"},
		},
		{
			id:     "RT-11 respaldo usa el '/' específico del método si existe",
			routes: []config.Route{{Endpoint: "/", Method: http.MethodGet, Backend: "http://web-get"}, root, {Endpoint: "/graph/", Method: http.MethodGet, Backend: "http://graph"}},
			method: http.MethodGet, target: "/index.html",
			want: expect{true, "/"},
		},
		{
			id:     "RT-19 hay lista del método sin '/' al inicio: respaldo al '/' genérico",
			routes: []config.Route{root, {Endpoint: "/dav/", Method: http.MethodGet, Backend: "http://dav"}},
			method: http.MethodGet, target: "/index.html",
			want: expect{true, "/"},
		},
		{
			id:     "RT-13 '/' declarada en segundo lugar: no hay respaldo",
			routes: []config.Route{{Endpoint: "/graph/", Backend: "http://graph"}, root},
			method: http.MethodGet, target: "/index.html",
			want: expect{false, ""},
		},
		{
			id:     "RT-14 sin '/' y sin coincidencia: falla sin pánico",
			routes: []config.Route{{Endpoint: "/graph/", Backend: "http://graph"}},
			method: http.MethodGet, target: "/index.html",
			want: expect{false, ""},
		},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			rt := newRouter(nil, c.routes...)
			ri, ok := rt.Route(httptest.NewRequest(c.method, c.target, nil))
			if ok != c.want.ok || ri.endpoint != c.want.endpoint {
				t.Errorf("Route(%s %s) = (%q, %v), esperado (%q, %v)", c.method, c.target, ri.endpoint, ok, c.want.endpoint, c.want.ok)
			}
			if !ok && (ri.endpoint != "" || ri.rewrite != nil) {
				t.Errorf("si Route falla debe devolver RoutingInfo vacío, se obtuvo %+v", ri)
			}
		})
	}

	t.Run("RT-01 error del selector de políticas", func(t *testing.T) {
		l, buf := spyLogger(t)
		rt := New(nil, staticPolicy, []config.Policy{{Name: "p", Routes: []config.Route{root}}}, l)
		rt.policySelector = func(*http.Request) (string, error) { return "", errors.New("selector caído") } // stub
		ri, ok := rt.Route(httptest.NewRequest(http.MethodGet, "/", nil))
		if ok || ri.endpoint != "" {
			t.Errorf("esperado (noInfo, false), se obtuvo (%q, %v)", ri.endpoint, ok)
		}
		if !strings.Contains(buf.String(), "Error while selecting pol") {
			t.Errorf("se esperaba el error en el log, se obtuvo %q", buf.String())
		}
	})

	t.Run("RT-02 política seleccionada no configurada", func(t *testing.T) {
		rt := newRouter(nil, root)
		rt.policySelector = func(*http.Request) (string, error) { return "inexistente", nil } // stub
		if ri, ok := rt.Route(httptest.NewRequest(http.MethodGet, "/", nil)); ok || ri.endpoint != "" {
			t.Errorf("esperado (noInfo, false), se obtuvo (%q, %v)", ri.endpoint, ok)
		}
	})

	t.Run("RT-12 la ruta elegida conserva sus atributos", func(t *testing.T) {
		rt := newRouter(nil, root,
			config.Route{Endpoint: "/status", Service: "eu.opencloud.web.frontend", Unprotected: true},
			config.Route{Endpoint: "/wopi", Backend: "http://collab", RemoteUserHeader: "X-Remote-User", SkipXAccessToken: true},
			config.Route{Endpoint: "/graph/", Backend: "http://graph"},
		)
		ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/status", nil))
		if !ri.IsRouteUnprotected() || ri.Service() != "eu.opencloud.web.frontend" {
			t.Errorf("/status: unprotected=%v service=%q", ri.IsRouteUnprotected(), ri.Service())
		}
		ri, _ = rt.Route(httptest.NewRequest(http.MethodGet, "/wopi/files/1", nil))
		if ri.RemoteUserHeader() != "X-Remote-User" || !ri.SkipXAccessToken() || ri.IsRouteUnprotected() {
			t.Errorf("/wopi: header=%q skip=%v unprotected=%v", ri.RemoteUserHeader(), ri.SkipXAccessToken(), ri.IsRouteUnprotected())
		}
		ri, _ = rt.Route(httptest.NewRequest(http.MethodGet, "/graph/v1.0/me", nil))
		if ri.IsRouteUnprotected() || ri.SkipXAccessToken() || ri.RemoteUserHeader() != "" {
			t.Errorf("/graph/: no debe heredar atributos de otras rutas: %+v", ri)
		}
	})
}

// ---------------------------------------------------------------------------
// Route con la configuración por defecto (decisiones de protección reales)
// ---------------------------------------------------------------------------

func TestContractRouteDefaultPolicies(t *testing.T) {
	rt := New(nil, nil, defaults.DefaultPolicies(), log.NopLogger())
	cases := []struct {
		id, method, target, service string
		unprotected                 bool
	}{
		{"DC-01 graph es protegido", "GET", "/graph/v1.0/me", "eu.opencloud.web.graph", false},
		{"DC-02 guestLinks (más específico, declarado antes) sin protección", "GET", "/graph/v1beta1/extensions/org.libregraph/guestLinks", "eu.opencloud.web.auth-guest", true},
		{"DC-03 invitations va a su servicio", "GET", "/graph/v1.0/invitations", "eu.opencloud.web.invitations", false},
		{"DC-04 config de ocs (regex) sin protección", "GET", "/ocs/v1.php/config", "eu.opencloud.web.frontend", true},
		{"DC-05 signing-key de ocs (regex) protegido", "GET", "/ocs/v2.php/cloud/user/signing-key", "eu.opencloud.web.ocs", false},
		{"DC-06 notificaciones SSE antes que notificaciones", "GET", "/ocs/v2.php/apps/notifications/api/v1/notifications/sse", "eu.opencloud.sse.sse", false},
		{"DC-07 previsualización (query) va a webdav", "GET", "/dav/files/admin/a.png?preview=1", "eu.opencloud.web.webdav", false},
		{"DC-08 REPORT (regex con método) va a webdav", "REPORT", "/remote.php/dav/files/admin", "eu.opencloud.web.webdav", false},
		{"DC-09 PROPFIND va a frontend", "PROPFIND", "/remote.php/dav/files/admin", "eu.opencloud.web.frontend", false},
		{"DC-10 '..' no convierte una ruta protegida en pública", "GET", "/konnect/../graph/v1.0/users", "eu.opencloud.web.graph", false},
		{"DC-11 status.php sin protección", "GET", "/status.php", "eu.opencloud.web.frontend", true},
		{"DC-12 ruta desconocida cae en web (respaldo '/')", "GET", "/files/spaces/personal", "eu.opencloud.web.web", true},
		{"DC-13 app/list sin protección, resto de /app/ protegido", "GET", "/app/list", "eu.opencloud.web.frontend", true},
		{"DC-14 app/open protegido", "POST", "/app/open", "eu.opencloud.web.frontend", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			ri, ok := rt.Route(httptest.NewRequest(c.method, c.target, nil))
			if !ok {
				t.Fatalf("Route(%s %s) falló", c.method, c.target)
			}
			if ri.Service() != c.service || ri.IsRouteUnprotected() != c.unprotected {
				t.Errorf("Route(%s %s) = (%s, unprotected=%v), esperado (%s, unprotected=%v)",
					c.method, c.target, ri.Service(), ri.IsRouteUnprotected(), c.service, c.unprotected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rewrite hook — aislamiento del selector de servicios con un stub
// ---------------------------------------------------------------------------

func proxyRequest(target string, header http.Header) *httputil.ProxyRequest {
	in := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		in.Header[k] = v
	}
	return &httputil.ProxyRequest{In: in, Out: in.Clone(context.Background())}
}

func TestContractRewrite(t *testing.T) {
	t.Run("RW-01 servicio: host y esquema del nodo elegido", func(t *testing.T) {
		sel := &stubServiceSelector{node: &registry.Node{Address: "10.0.0.7:9200", Metadata: map[string]string{"protocol": "http"}}}
		rt := newRouter(sel, config.Route{Endpoint: "/", Service: "eu.opencloud.web.web"})
		ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/x", nil))
		pr := proxyRequest("http://proxy.local/x", nil)
		ri.Rewrite()(pr)
		if pr.Out.URL.Host != "10.0.0.7:9200" || pr.Out.URL.Scheme != "http" {
			t.Errorf("Out.URL = %s://%s", pr.Out.URL.Scheme, pr.Out.URL.Host)
		}
		if len(sel.asked) != 1 || sel.asked[0] != "eu.opencloud.web.web" {
			t.Errorf("servicios pedidos al selector = %v", sel.asked)
		}
	})

	t.Run("RW-02 servicio con use_tls=true usa https", func(t *testing.T) {
		sel := &stubServiceSelector{node: &registry.Node{Address: "10.0.0.7:9200", Metadata: map[string]string{"protocol": "http", "use_tls": "true"}}}
		rt := newRouter(sel, config.Route{Endpoint: "/", Service: "svc"})
		ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/x", nil))
		pr := proxyRequest("http://proxy.local/x", nil)
		ri.Rewrite()(pr)
		if pr.Out.URL.Scheme != "https" {
			t.Errorf("Out.URL.Scheme = %q, esperado https", pr.Out.URL.Scheme)
		}
	})

	for _, c := range []struct {
		id  string
		sel *stubServiceSelector
		msg string
	}{
		{"RW-03 Select falla: no reescribe y registra el error", &stubServiceSelector{selectErr: errors.New("sin registro")}, "could not select service from the registry"},
		{"RW-04 Next falla: no reescribe y registra el error", &stubServiceSelector{nextErr: errors.New("sin nodos")}, "could not select next node"},
	} {
		t.Run(c.id, func(t *testing.T) {
			l, buf := spyLogger(t)
			rt := New(c.sel, staticPolicy, []config.Policy{{Name: "p", Routes: []config.Route{{Endpoint: "/", Service: "svc"}}}}, l)
			ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/x", nil))
			pr := proxyRequest("http://proxy.local/x", nil)
			ri.Rewrite()(pr)
			if pr.Out.URL.Host != "proxy.local" || pr.Out.Header.Get("X-Forwarded-Host") != "" {
				t.Errorf("la petición no debía modificarse: host=%q headers=%v", pr.Out.URL.Host, pr.Out.Header)
			}
			if !strings.Contains(buf.String(), c.msg) {
				t.Errorf("se esperaba %q en el log, se obtuvo %q", c.msg, buf.String())
			}
		})
	}

	t.Run("RW-05 backend estático: host, ruta, query, vhost, cabeceras y User-Agent vacío", func(t *testing.T) {
		rt := newRouter(nil, config.Route{
			Endpoint: "/", Backend: "http://backend:8080/base?b=2",
			ApacheVHost: true, AdditionalHeaders: map[string]string{"X-Extra": "1"},
		})
		ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/graph/me?x=1", nil))
		pr := proxyRequest("http://proxy.local/graph/me?x=1", nil)
		ri.Rewrite()(pr)
		out := pr.Out
		checks := map[string][2]string{
			"scheme":     {out.URL.Scheme, "http"},
			"url host":   {out.URL.Host, "backend:8080"},
			"host":       {out.Host, "backend:8080"},
			"path":       {out.URL.Path, "/base/graph/me"},
			"query":      {out.URL.RawQuery, "b=2&x=1"},
			"X-Extra":    {out.Header.Get("X-Extra"), "1"},
			"User-Agent": {out.Header.Get("User-Agent"), ""},
			"X-Fwd-Host": {out.Header.Get("X-Forwarded-Host"), "proxy.local"},
		}
		for name, v := range checks {
			if v[0] != v[1] {
				t.Errorf("%s = %q, esperado %q", name, v[0], v[1])
			}
		}
		if _, present := out.Header["User-Agent"]; !present {
			t.Errorf("User-Agent debe quedar presente y vacío para que Go no ponga el suyo")
		}
	})

	t.Run("RW-06 backend sin query ni vhost conserva Host y User-Agent", func(t *testing.T) {
		rt := newRouter(nil, config.Route{Endpoint: "/", Backend: "http://backend:8080"})
		ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/a?x=1", nil))
		pr := proxyRequest("http://proxy.local/a?x=1", http.Header{"User-Agent": {"curl/8"}})
		ri.Rewrite()(pr)
		if pr.Out.Host != "proxy.local" || pr.Out.URL.RawQuery != "x=1" || pr.Out.Header.Get("User-Agent") != "curl/8" {
			t.Errorf("host=%q query=%q ua=%q", pr.Out.Host, pr.Out.URL.RawQuery, pr.Out.Header.Get("User-Agent"))
		}
	})

	t.Run("RW-07 backend con query y petición sin query", func(t *testing.T) {
		rt := newRouter(nil, config.Route{Endpoint: "/", Backend: "http://backend:8080?b=2"})
		ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/a", nil))
		pr := proxyRequest("http://proxy.local/a", nil)
		ri.Rewrite()(pr)
		if pr.Out.URL.RawQuery != "b=2" {
			t.Errorf("query = %q, esperado b=2", pr.Out.URL.RawQuery)
		}
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func TestContractMiddleware(t *testing.T) {
	policies := []config.Policy{{Name: "p", Routes: []config.Route{{Endpoint: "/graph/", Backend: "http://graph"}}}}

	t.Run("MW-01 si el ruteo falla: 500, se notifica y no se llama al siguiente", func(t *testing.T) {
		failed, called := 0, 0
		h := Middleware(nil, staticPolicy, policies, func(*http.Request) { failed++ }, log.NopLogger())(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nada", nil))
		if rec.Code != http.StatusInternalServerError || failed != 1 || called != 0 {
			t.Errorf("code=%d failed=%d called=%d", rec.Code, failed, called)
		}
	})

	t.Run("MW-02 si el ruteo funciona: siguiente handler con RoutingInfo en el contexto", func(t *testing.T) {
		failed := 0
		var got RoutingInfo
		h := Middleware(nil, staticPolicy, policies, func(*http.Request) { failed++ }, log.NopLogger())(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ContextRoutingInfo(r.Context()) }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/graph/v1.0/me", nil))
		if rec.Code != http.StatusOK || failed != 0 || got.endpoint != "/graph/" {
			t.Errorf("code=%d failed=%d endpoint=%q", rec.Code, failed, got.endpoint)
		}
	})

	t.Run("MW-03 ContextRoutingInfo sin valor previo entra en pánico (precondición)", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Errorf("se esperaba pánico")
			}
		}()
		ContextRoutingInfo(context.Background())
	})
}

// ---------------------------------------------------------------------------
// New — comportamiento excepcional (logger.Fatal interceptado)
// ---------------------------------------------------------------------------

type fatalCalled struct{}

// expectFatal ejecuta build con un logger spy y sustituye la salida del
// proceso de zerolog por un pánico controlado, para poder verificar las
// ramas que llaman a logger.Fatal sin terminar el proceso de pruebas.
func expectFatal(t *testing.T, build func(l log.Logger)) string {
	t.Helper()
	l, buf := spyLogger(t)
	old := zerolog.FatalExitFunc
	zerolog.FatalExitFunc = func() { panic(fatalCalled{}) }
	defer func() { zerolog.FatalExitFunc = old }()

	fatal := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(fatalCalled); !ok {
					panic(r)
				}
				fatal = true
			}
		}()
		build(l)
	}()
	if !fatal {
		t.Fatalf("se esperaba logger.Fatal; log: %s", buf.String())
	}
	return buf.String()
}

func TestContractNew(t *testing.T) {
	t.Run("NW-01 sin selector de políticas se usa la primera política", func(t *testing.T) {
		rt := New(nil, nil, []config.Policy{
			{Name: "primera", Routes: []config.Route{{Endpoint: "/", Backend: "http://uno"}}},
			{Name: "segunda", Routes: []config.Route{{Endpoint: "/", Backend: "http://dos"}}},
		}, log.NopLogger())
		pol, err := rt.policySelector(httptest.NewRequest(http.MethodGet, "/", nil))
		if err != nil || pol != "primera" {
			t.Errorf("política = %q, err = %v", pol, err)
		}
	})

	t.Run("NW-02 ruta sin Backend ni Service: Fatal", func(t *testing.T) {
		out := expectFatal(t, func(l log.Logger) {
			New(nil, staticPolicy, []config.Policy{{Name: "p", Routes: []config.Route{{Endpoint: "/x"}}}}, l)
		})
		if !strings.Contains(out, "neither Backend nor Service is set") {
			t.Errorf("mensaje inesperado: %s", out)
		}
	})

	t.Run("NW-03 Backend con URL malformada: Fatal", func(t *testing.T) {
		out := expectFatal(t, func(l log.Logger) {
			New(nil, staticPolicy, []config.Policy{{Name: "p", Routes: []config.Route{{Endpoint: "/x", Backend: "http://[::1"}}}}, l)
		})
		if !strings.Contains(out, "malformed url") {
			t.Errorf("mensaje inesperado: %s", out)
		}
	})

	t.Run("NW-04 configuración de selector de políticas inválida: Fatal", func(t *testing.T) {
		out := expectFatal(t, func(l log.Logger) {
			New(nil, &config.PolicySelector{}, []config.Policy{{Name: "p", Routes: []config.Route{{Endpoint: "/", Backend: "http://a"}}}}, l)
		})
		if !strings.Contains(out, "Could not load policy-selector") {
			t.Errorf("mensaje inesperado: %s", out)
		}
	})

	t.Run("NW-05 sin políticas y sin selector: pánico (precondición no validada)", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Errorf("se esperaba pánico por policies[0]")
			}
		}()
		New(nil, nil, nil, log.NopLogger())
	})

	t.Run("NW-06 tipo vacío se registra como prefix y conserva el método", func(t *testing.T) {
		rt := newRouter(nil, config.Route{Endpoint: "/a/", Backend: "http://a"}, config.Route{Endpoint: "/b/", Method: "PUT", Backend: "http://b"})
		if n := len(rt.rewriters["p"][config.PrefixRoute][""]); n != 1 {
			t.Errorf("rutas prefix genéricas = %d, esperado 1", n)
		}
		if n := len(rt.rewriters["p"][config.PrefixRoute]["PUT"]); n != 1 {
			t.Errorf("rutas prefix PUT = %d, esperado 1", n)
		}
	})
}
