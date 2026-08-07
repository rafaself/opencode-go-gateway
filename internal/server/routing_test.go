package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/codex"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

const routedTextStream = `data: {"id":"provider","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"routed"},"finish_reason":null}]}
data: {"id":"provider","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]

`

func routedUpstream(called *atomic.Bool) UpstreamClient {
	return UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		called.Store(true)
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(routedTextStream))}, nil
	})
}

func taggedTextRequestBody(model string) string {
	return `{"model":"` + model + `","instructions":"system instruction","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`
}

func postTaggedRequest(t *testing.T, gateway *httptest.Server, body string) *http.Response {
	t.Helper()
	return postRequest(t, gateway, body)
}

func finalResponseObject(t *testing.T, body []map[string]any) map[string]any {
	t.Helper()
	for index := len(body) - 1; index >= 0; index-- {
		value, ok := body[index]["response"].(map[string]any)
		if ok {
			return value
		}
	}
	t.Fatalf("no response object in events: %s", mustJSON(t, body))
	return nil
}

func TestRoutingGoTagUsesGoUpstreamAndGoModel(t *testing.T) {
	var goCalled, zenCalled atomic.Bool
	goClient := routedUpstream(&goCalled)
	zenClient := routedUpstream(&zenCalled)
	server, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: goClient, ZenUpstream: zenClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	gateway := httptest.NewServer(server)
	t.Cleanup(gateway.Close)

	response := postTaggedRequest(t, gateway, taggedTextRequestBody(opencodego.TaggedModel(opencodego.DefaultModel, opencodego.ProviderTagGo)))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !goCalled.Load() || zenCalled.Load() {
		t.Fatalf("routing = go:%v zen:%v, want only go", goCalled.Load(), zenCalled.Load())
	}
	events := readResponseEvents(t, response.Body)
	object := finalResponseObject(t, events)
	if object["model"] != opencodego.DefaultModel {
		t.Fatalf("response model = %#v, want %q", object["model"], opencodego.DefaultModel)
	}
}

func TestRoutingZenTagUsesZenUpstreamAndZenModel(t *testing.T) {
	var goCalled, zenCalled atomic.Bool
	goClient := routedUpstream(&goCalled)
	zenClient := routedUpstream(&zenCalled)
	server, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: goClient, ZenUpstream: zenClient, ZenModel: opencodego.DeepSeekV4FlashFreeModel}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	gateway := httptest.NewServer(server)
	t.Cleanup(gateway.Close)

	response := postTaggedRequest(t, gateway, taggedTextRequestBody(opencodego.TaggedModel(opencodego.DefaultModel, opencodego.ProviderTagZen)))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !zenCalled.Load() || goCalled.Load() {
		t.Fatalf("routing = go:%v zen:%v, want only zen", goCalled.Load(), zenCalled.Load())
	}
	events := readResponseEvents(t, response.Body)
	object := finalResponseObject(t, events)
	if object["model"] != opencodego.DeepSeekV4FlashFreeModel {
		t.Fatalf("response model = %#v, want %q", object["model"], opencodego.DeepSeekV4FlashFreeModel)
	}
}

func TestRoutingRejectsModelsWithoutARecognizedTag(t *testing.T) {
	for _, test := range []struct {
		name  string
		model string
	}{
		{name: "untagged", model: "gpt-5.3-codex"},
		{name: "unknown tag", model: "deepseek-v4-flash (pro)"},
		{name: "empty", model: ""},
		{name: "malformed", model: "deepseek-v4-flash (go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var goCalled atomic.Bool
			gateway := newIntegrationGateway(t, routedUpstream(&goCalled), nil)
			response := postTaggedRequest(t, gateway, taggedTextRequestBody(test.model))
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			var errorBody struct {
				Error struct {
					Code  string `json:"code"`
					Param string `json:"param"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&errorBody); err != nil {
				t.Fatal(err)
			}
			if errorBody.Error.Code != string(codex.ErrorInvalidRequest) || errorBody.Error.Param != "model" {
				t.Fatalf("error = %+v, want invalid_request with param model", errorBody)
			}
			if goCalled.Load() {
				t.Fatal("upstream was invoked for an unroutable model")
			}
		})
	}
}

func TestRoutingZenBackendDefaultsToNotConfigured(t *testing.T) {
	var goCalled atomic.Bool
	gateway := newIntegrationGateway(t, routedUpstream(&goCalled), nil)
	response := postTaggedRequest(t, gateway, taggedTextRequestBody(opencodego.TaggedModel(opencodego.DefaultModel, opencodego.ProviderTagZen)))
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	var errorBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&errorBody); err != nil {
		t.Fatal(err)
	}
	if errorBody.Error.Code != "upstream_not_configured" {
		t.Fatalf("error code = %q, want upstream_not_configured", errorBody.Error.Code)
	}
	if goCalled.Load() {
		t.Fatal("go upstream was invoked for a zen request")
	}
}
