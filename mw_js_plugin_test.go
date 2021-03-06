package main

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Sirupsen/logrus"
	prefixed "github.com/x-cray/logrus-prefixed-formatter"

	"github.com/TykTechnologies/tyk/apidef"
	"github.com/TykTechnologies/tyk/config"
	logger "github.com/TykTechnologies/tyk/log"
)

func TestJSVMLogs(t *testing.T) {
	var buf bytes.Buffer
	jsvm := JSVM{}
	jsvm.Init(nil)
	jsvm.Log = logrus.New()
	jsvm.Log.Out = &buf
	jsvm.Log.Formatter = new(prefixed.TextFormatter)

	jsvm.RawLog = logrus.New()
	jsvm.RawLog.Out = &buf
	jsvm.RawLog.Formatter = new(logger.RawFormatter)

	const in = `
log("foo")
log('{"x": "y"}')
rawlog("foo")
rawlog('{"x": "y"}')
`
	// note how the logger leaves spaces at the end
	want := []string{
		`time=TIME level=info msg=foo type=log-msg `,
		`time=TIME level=info msg="{\"x\": \"y\"}" type=log-msg `,
		`foo`,
		`{"x": "y"}`,
	}
	if _, err := jsvm.VM.Run(in); err != nil {
		t.Fatalf("failed to run js: %v", err)
	}
	got := strings.Split(strings.Trim(buf.String(), "\n"), "\n")
	i := 0
	timeRe := regexp.MustCompile(`time="[^"]*"`)
	for _, line := range got {
		if i >= len(want) {
			t.Logf("too many lines")
			t.Fail()
			break
		}
		s := timeRe.ReplaceAllString(line, "time=TIME")
		if s != line && !strings.Contains(s, "type=log-msg") {
			continue // log line from elsewhere (async)
		}
		if s != want[i] {
			t.Logf("%s != %s", s, want[i])
			t.Fail()
		}
		i++
	}
}

func TestJSVMBody(t *testing.T) {
	dynMid := &DynamicMiddleware{
		BaseMiddleware: BaseMiddleware{
			Spec: &APISpec{APIDefinition: &apidef.APIDefinition{}},
		},
		MiddlewareClassName: "leakMid",
		Pre:                 true,
	}
	body := "foô \uffff \u0000 \xff bàr"
	req := httptest.NewRequest("GET", "/foo", strings.NewReader(body))
	jsvm := JSVM{}
	jsvm.Init(nil)

	const js = `
var leakMid = new TykJS.TykMiddleware.NewMiddleware({})

leakMid.NewProcessRequest(function(request, session) {
	request.Body += " appended"
	return leakMid.ReturnData(request, session.meta_data)
});`
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm
	dynMid.ProcessRequest(nil, req, nil)

	bs, err := ioutil.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read final body: %v", err)
	}
	want := body + " appended"
	if got := string(bs); want != got {
		t.Fatalf("JS plugin broke non-UTF8 body %q into %q",
			want, got)
	}
}

func TestJSVMProcessTimeout(t *testing.T) {
	dynMid := &DynamicMiddleware{
		BaseMiddleware: BaseMiddleware{
			Spec: &APISpec{APIDefinition: &apidef.APIDefinition{}},
		},
		MiddlewareClassName: "leakMid",
		Pre:                 true,
	}
	req := httptest.NewRequest("GET", "/foo", strings.NewReader("body"))
	jsvm := JSVM{}
	jsvm.Init(nil)
	jsvm.Timeout = time.Millisecond

	// this js plugin just loops forever, keeping Otto at 100% CPU
	// usage and running forever.
	const js = `
var leakMid = new TykJS.TykMiddleware.NewMiddleware({})

leakMid.NewProcessRequest(function(request, session) {
	while (true) {
	}
	return leakMid.ReturnData(request, session.meta_data)
});`
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm

	done := make(chan bool)
	go func() {
		dynMid.ProcessRequest(nil, req, nil)
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("js vm wasn't killed after its timeout")
	}
}

func TestJSVMConfigData(t *testing.T) {
	spec := &APISpec{APIDefinition: &apidef.APIDefinition{}}
	spec.ConfigData = map[string]interface{}{
		"foo": "bar",
	}
	const js = `
var testJSVMData = new TykJS.TykMiddleware.NewMiddleware({})

testJSVMData.NewProcessRequest(function(request, session, spec) {
	request.SetHeaders["data-foo"] = spec.config_data.foo
	return testJSVMData.ReturnData(request, {})
});`
	dynMid := &DynamicMiddleware{
		BaseMiddleware:      BaseMiddleware{spec, nil},
		MiddlewareClassName: "testJSVMData",
		Pre:                 true,
	}
	jsvm := JSVM{}
	jsvm.Init(nil)
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm

	r := testReq(t, "GET", "/v1/test-data", nil)
	dynMid.ProcessRequest(nil, r, nil)
	if want, got := "bar", r.Header.Get("data-foo"); want != got {
		t.Fatalf("wanted header to be %q, got %q", want, got)
	}
}

func TestJSVMReturnOverridesFullResponse(t *testing.T) {
	spec := &APISpec{APIDefinition: &apidef.APIDefinition{}}
	spec.ConfigData = map[string]interface{}{
		"foo": "bar",
	}
	const js = `
var testJSVMData = new TykJS.TykMiddleware.NewMiddleware({})

testJSVMData.NewProcessRequest(function(request, session, config) {
	request.ReturnOverrides.ResponseError = "Foobarbaz"
	request.ReturnOverrides.ResponseCode = 200
	request.ReturnOverrides.ResponseHeaders = {
		"X-Foo": "Bar",
		"X-Baz": "Qux"
	}
	return testJSVMData.ReturnData(request, {})
});`
	dynMid := &DynamicMiddleware{
		BaseMiddleware:      BaseMiddleware{spec, nil},
		MiddlewareClassName: "testJSVMData",
		Pre:                 true,
	}
	jsvm := JSVM{}
	jsvm.Init(nil)
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm

	rec := httptest.NewRecorder()
	r := testReq(t, "GET", "/v1/test-data", nil)
	dynMid.ProcessRequest(rec, r, nil)

	wantBody := "Foobarbaz"
	gotBody := rec.Body.String()
	if wantBody != gotBody {
		t.Fatalf("wanted body to be %q, got %q", wantBody, gotBody)
	}
	if want, got := "Bar", rec.HeaderMap.Get("x-foo"); got != want {
		t.Fatalf("wanted header to be %q, got %q", want, got)
	}
	if want, got := "Qux", rec.HeaderMap.Get("x-baz"); got != want {
		t.Fatalf("wanted header to be %q, got %q", want, got)
	}

	if want := 200; rec.Code != 200 {
		t.Fatalf("wanted code to be %d, got %d", want, rec.Code)
	}
}

func TestJSVMReturnOverridesError(t *testing.T) {
	spec := &APISpec{APIDefinition: &apidef.APIDefinition{}}
	spec.ConfigData = map[string]interface{}{
		"foo": "bar",
	}
	const js = `
var testJSVMData = new TykJS.TykMiddleware.NewMiddleware({})

testJSVMData.NewProcessRequest(function(request, session, config) {
	request.ReturnOverrides.ResponseError = "Foobarbaz"
	request.ReturnOverrides.ResponseCode = 401
	return testJSVMData.ReturnData(request, {})
});`
	dynMid := &DynamicMiddleware{
		BaseMiddleware:      BaseMiddleware{spec, nil},
		MiddlewareClassName: "testJSVMData",
		Pre:                 true,
	}
	jsvm := JSVM{}
	jsvm.Init(nil)
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm

	r := testReq(t, "GET", "/v1/test-data", nil)
	err, code := dynMid.ProcessRequest(nil, r, nil)

	if want := 401; code != 401 {
		t.Fatalf("wanted code to be %d, got %d", want, code)
	}

	wantBody := "Foobarbaz"
	if !strings.Contains(err.Error(), wantBody) {
		t.Fatalf("wanted body to contain to be %v, got %v", wantBody, err.Error())
	}
}

func TestJSVMUserCore(t *testing.T) {
	spec := &APISpec{APIDefinition: &apidef.APIDefinition{}}
	const js = `
var testJSVMCore = new TykJS.TykMiddleware.NewMiddleware({})

testJSVMCore.NewProcessRequest(function(request, session, config) {
	request.SetHeaders["global"] = globalVar
	return testJSVMCore.ReturnData(request, {})
});`
	dynMid := &DynamicMiddleware{
		BaseMiddleware:      BaseMiddleware{spec, nil},
		MiddlewareClassName: "testJSVMCore",
		Pre:                 true,
	}
	tfile, err := ioutil.TempFile("", "tykjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tfile, `var globalVar = "globalValue"`); err != nil {
		t.Fatal(err)
	}
	globalConf := config.Global()
	old := globalConf.TykJSPath
	globalConf.TykJSPath = tfile.Name()
	config.SetGlobal(globalConf)
	defer func() {
		globalConf.TykJSPath = old
		config.SetGlobal(globalConf)
	}()
	jsvm := JSVM{}
	jsvm.Init(nil)
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm

	r := testReq(t, "GET", "/foo", nil)
	dynMid.ProcessRequest(nil, r, nil)

	if want, got := "globalValue", r.Header.Get("global"); want != got {
		t.Fatalf("wanted header to be %q, got %q", want, got)
	}
}
func TestJSVMRequestScheme(t *testing.T) {
	dynMid := &DynamicMiddleware{
		BaseMiddleware: BaseMiddleware{
			Spec: &APISpec{APIDefinition: &apidef.APIDefinition{}},
		},
		MiddlewareClassName: "leakMid",
		Pre:                 true,
	}
	req := httptest.NewRequest("GET", "/foo", nil)
	req.URL.Scheme = "http"
	jsvm := JSVM{}
	jsvm.Init(nil)

	const js = `
var leakMid = new TykJS.TykMiddleware.NewMiddleware({})
leakMid.NewProcessRequest(function(request, session) {
	var test = request.Scheme += " appended"
	var responseObject = {
        Body: test,
        Code: 200
    }
	return leakMid.ReturnData(responseObject, session.meta_data)
});`
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm
	dynMid.ProcessRequest(nil, req, nil)

	bs, err := ioutil.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read final body: %v", err)
	}
	want := "http" + " appended"
	if got := string(bs); want != got {
		t.Fatalf("JS plugin broke non-UTF8 body %q into %q",
			want, got)
	}
}

func TestTykMakeHTTPRequest(t *testing.T) {
	ts := newTykTestServer()
	defer ts.Close()

	spec := &APISpec{APIDefinition: &apidef.APIDefinition{}}
	spec.ConfigData = map[string]interface{}{
		"base_url": ts.URL,
	}

	const js = `
	var testTykMakeHTTPRequest = new TykJS.TykMiddleware.NewMiddleware({})
	
	testTykMakeHTTPRequest.NewProcessRequest(function(request, session, spec) {
		var newRequest = {
			"Method": "GET",
			"Headers": {"Accept": "application/json"},
			"Domain": spec.config_data.base_url,
			"Resource": "/sample"
		}
	
		var resp = TykMakeHttpRequest(JSON.stringify(newRequest));
		var useableResponse = JSON.parse(resp);
	
		if(useableResponse.Code > 400) {
			request.ReturnOverrides.ResponseCode = useableResponse.code
			request.ReturnOverrides.ResponseError = "error"
		}
		
		return testTykMakeHTTPRequest.ReturnData(request, {})
	});`

	dynMid := &DynamicMiddleware{
		BaseMiddleware:      BaseMiddleware{spec, nil},
		MiddlewareClassName: "testTykMakeHTTPRequest",
		Pre:                 true,
	}
	jsvm := JSVM{}
	jsvm.Init(nil)
	if _, err := jsvm.VM.Run(js); err != nil {
		t.Fatalf("failed to set up js plugin: %v", err)
	}
	dynMid.Spec.JSVM = jsvm

	t.Run("Existing endpoint", func(t *testing.T) {
		buildAndLoadAPI(func(spec *APISpec) {
			spec.Proxy.ListenPath = "/sample"
		})

		r := testReq(t, "GET", "/v1/test-data", nil)
		_, code := dynMid.ProcessRequest(nil, r, nil)
		if want := 200; code != 200 {
			t.Fatalf("wanted code to be %d, got %d", want, code)
		}
	})

	t.Run("Nonexistent endpoint", func(t *testing.T) {
		buildAndLoadAPI(func(spec *APISpec) {
			spec.Proxy.ListenPath = "/abc"
		})

		r := testReq(t, "GET", "/v1/test-data", nil)
		_, code := dynMid.ProcessRequest(nil, r, nil)
		if want := 404; code != 404 {
			t.Fatalf("wanted code to be %d, got %d", want, code)
		}
	})
}
