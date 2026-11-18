// Package cdn limpa o cache da CDN quando um arquivo é apagado, para que um
// link removido (ex.: pedido de remoção/LGPD) pare de responder na borda.
package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Purger remove URLs do cache da CDN.
type Purger interface {
	Purge(ctx context.Context, urls []string) error
}

// DefaultCloudflareAPI é a base da API v4 da Cloudflare.
const DefaultCloudflareAPI = "https://api.cloudflare.com/client/v4"

// Cloudflare purga por URL numa zona. O token precisa da permissão
// "Zone > Cache Purge > Purge" e nunca aparece em erros ou logs.
type Cloudflare struct {
	ZoneID  string
	Token   string
	APIBase string       // vazio = DefaultCloudflareAPI (testes trocam)
	Client  *http.Client // nil = cliente com timeout de 15s
}

type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Purge chama POST /zones/{zone}/purge_cache com {"files": urls}.
func (c *Cloudflare) Purge(ctx context.Context, urls []string) error {
	base := c.APIBase
	if base == "" {
		base = DefaultCloudflareAPI
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(map[string][]string{"files": urls})
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(base, "/") + "/zones/" + url.PathEscape(c.ZoneID) + "/purge_cache"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	var r cfResponse
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r)
	if resp.StatusCode != http.StatusOK || !r.Success {
		msg := ""
		for _, e := range r.Errors {
			msg += fmt.Sprintf(" [%d] %s", e.Code, e.Message)
		}
		return fmt.Errorf("cloudflare: status %d%s", resp.StatusCode, msg)
	}
	return nil
}
