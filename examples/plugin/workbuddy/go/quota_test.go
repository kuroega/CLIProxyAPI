package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestParseCredits(t *testing.T) {
	packages, err := parseCredits([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageName":"Daily","CycleCapacitySize":500,"CycleCapacityRemain":470,"CycleCapacityUsed":35},
		{"PackageName":"Monthly","CapacitySize":100,"CapacityRemain":60,"CapacityUsed":40}
	]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || packages[0].Remaining != 465 || packages[0].Used != 35 || packages[1].Remaining != 60 {
		t.Fatalf("unexpected credit packages: %+v", packages)
	}
}

func TestParseCreditsRejectsMissingOrFailedResponse(t *testing.T) {
	for _, raw := range []string{
		`{"code":1,"data":{"Response":{"Data":{"Accounts":[]}}}}`,
		`{"code":0,"data":{"Response":{"Data":{}}}}`,
		`{}`, `not json`,
	} {
		if _, err := parseCredits([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid quota response %s", raw)
		}
	}
}

func TestFetchQuotaUsesCredentialAndBillingEndpoint(t *testing.T) {
	storage, _ := json.Marshal(credential{Provider: provider, UID: "alice", Realm: "cn", AccessToken: "private-token"})
	request, _ := json.Marshal(quotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: provider, StorageJSON: storage}, HostCallbackID: "callback"})
	call := func(method string, payload any, result any) error {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("unexpected callback %s", method)
		}
		req := payload.(hostRequest)
		if req.URL != "https://www.codebuddy.cn/v2/billing/meter/get-user-resource" || req.HostCallbackID != "callback" || req.Headers.Get("Authorization") != "Bearer private-token" || req.Headers.Get("X-User-Id") != "alice" {
			t.Fatalf("incorrect billing request: url=%q callback=%q uid=%q", req.URL, req.HostCallbackID, req.Headers.Get("X-User-Id"))
		}
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil || body["ProductCode"] != "p_tcaca" {
			t.Fatalf("incorrect billing request body: %v", err)
		}
		*result.(*pluginapi.HTTPResponse) = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"Daily","CycleCapacitySize":500,"CycleCapacityRemain":450,"CycleCapacityUsed":50}]}}}}`)}
		return nil
	}
	raw, err := fetchQuota(request, call)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var quota pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(env.Result, &quota); err != nil {
		t.Fatal(err)
	}
	if len(quota.Summary) != 3 || quota.Summary[0].Value != 450 || quota.Summary[0].Unit != "credits" || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != 0.9 {
		t.Fatalf("incorrect normalized quota: %+v", quota)
	}
	if strings.Contains(string(raw), "private-token") {
		t.Fatal("quota result leaked access token")
	}
}
