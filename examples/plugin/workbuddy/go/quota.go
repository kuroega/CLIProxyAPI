package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type quotaRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type creditPackage struct {
	Name      string
	Remaining float64
	Used      float64
	Total     float64
}

func fetchQuota(raw []byte, call callback) ([]byte, error) {
	var req quotaRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode WorkBuddy quota request: %w", err)
	}
	if req.Provider != "" && req.Provider != provider {
		return nil, errors.New("quota credential is not a WorkBuddy credential")
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("decode WorkBuddy quota credential: %w", err)
	}
	now := time.Now().Local().Format("2006-01-02 15:04:05")
	body, err := json.Marshal(map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca",
		"Status": []int{0, 3}, "PackageEndTimeRangeBegin": now,
		"PackageEndTimeRangeEnd": "2036-01-01 00:00:00",
	})
	if err != nil {
		return nil, fmt.Errorf("encode WorkBuddy quota request: %w", err)
	}
	headers := commonHeaders(c.Realm)
	headers.Set("Authorization", "Bearer "+c.AccessToken)
	headers.Set("X-User-Id", c.UID)
	headers.Set("X-CodeBuddy-Request", "1")
	headers.Set("X-Request-ID", uuid.NewString())
	machine := md5.Sum([]byte("machine:" + c.UID))
	session := md5.Sum([]byte("session:" + c.UID))
	headers.Set("X-Machine-ID", hex.EncodeToString(machine[:]))
	headers.Set("X-Session-ID", hex.EncodeToString(session[:]))
	billingURL := "https://www.workbuddy.ai/v2/billing/meter/get-user-resource"
	if c.Realm == "cn" {
		billingURL = "https://www.codebuddy.cn/v2/billing/meter/get-user-resource"
		headers.Set("X-Domain", "copilot.tencent.com")
		headers.Set("X-Product", "SaaS")
		headers.Set("X-No-Enterprise-Id", "1")
		headers.Set("Accept-Language", "zh-CN")
	} else {
		headers.Set("X-Domain", "www.workbuddy.ai")
	}
	var upstream pluginapi.HTTPResponse
	if err := call(pluginabi.MethodHostHTTPDo, hostRequest{HTTPRequest: pluginapi.HTTPRequest{
		Method: http.MethodPost, URL: billingURL, Headers: headers, Body: body,
	}, HostCallbackID: req.HostCallbackID}, &upstream); err != nil {
		return nil, fmt.Errorf("fetch WorkBuddy credits: %w", err)
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		return nil, &upstreamError{upstream.StatusCode, fmt.Sprintf("WorkBuddy billing HTTP %d", upstream.StatusCode)}
	}
	packages, err := parseCredits(upstream.Body)
	if err != nil {
		return nil, err
	}
	resp := pluginapi.QuotaFetchResponse{}
	var total, used, remaining float64
	for _, pkg := range packages {
		remaining += pkg.Remaining
		used += pkg.Used
		total += pkg.Total
	}
	resp.Summary = []pluginapi.QuotaMetric{
		{Key: "remaining", Label: "Remaining", Value: remaining, Unit: "credits"},
		{Key: "used", Label: "Used", Value: used, Unit: "credits"},
		{Key: "total", Label: "Total", Value: total, Unit: "credits"},
	}
	for _, pkg := range packages {
		fraction := 0.0
		if pkg.Total > 0 {
			fraction = pkg.Remaining / pkg.Total
		}
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{
			DisplayName: pkg.Name,
			Buckets: []pluginapi.QuotaBucket{{Window: "credits", RemainingFraction: fraction,
				Description: fmt.Sprintf("%.0f / %.0f credits remaining", pkg.Remaining, pkg.Total)}},
		})
	}
	return success(resp)
}

// parseCredits follows the billing meter's cycle counters when available.
// Never treat an invalid or unexpected response as a zero balance.
func parseCredits(raw []byte) ([]creditPackage, error) {
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []struct {
						Name           string  `json:"PackageName"`
						CycleSize      float64 `json:"CycleCapacitySize"`
						CycleRemaining float64 `json:"CycleCapacityRemain"`
						CycleUsed      float64 `json:"CycleCapacityUsed"`
						Size           float64 `json:"CapacitySize"`
						Remaining      float64 `json:"CapacityRemain"`
						Used           float64 `json:"CapacityUsed"`
					} `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode WorkBuddy credits: %w", err)
	}
	if envelope.Code == nil || *envelope.Code != 0 {
		return nil, errors.New("WorkBuddy billing rejected the credits request")
	}
	if envelope.Data.Response.Data.Accounts == nil {
		return nil, errors.New("WorkBuddy billing response has no accounts field")
	}
	packages := make([]creditPackage, 0, len(envelope.Data.Response.Data.Accounts))
	for _, a := range envelope.Data.Response.Data.Accounts {
		pkg := creditPackage{Name: a.Name, Remaining: a.Remaining, Used: a.Used, Total: a.Size}
		if pkg.Name == "" {
			pkg.Name = "Package"
		}
		if a.CycleSize > 0 {
			pkg.Total = a.CycleSize
			pkg.Remaining = a.CycleRemaining
			pkg.Used = max(0, pkg.Total-pkg.Remaining)
			if a.CycleUsed > pkg.Used {
				pkg.Used = a.CycleUsed
				pkg.Remaining = max(0, pkg.Total-pkg.Used)
			}
		}
		if pkg.Total < 0 || pkg.Remaining < 0 || pkg.Used < 0 {
			return nil, errors.New("WorkBuddy billing returned negative credit counters")
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}
