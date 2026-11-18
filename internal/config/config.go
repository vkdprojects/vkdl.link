// Package config lê a configuração do servidor a partir de variáveis de ambiente.
package config

import (
	"crypto/rand"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config reúne tudo o que o servidor precisa saber para subir.
type Config struct {
	Listen             string
	BaseURL            string
	DataDir            string
	MaxStorage         int64
	MinFreeDisk        int64
	MaxUpload          int64
	AdminToken         string
	Secret             []byte
	SecretGenerated    bool // true quando VKDL_SECRET estava vazio e geramos um aleatório
	RateAnon           int
	RateKey            int
	TrustedProxyHeader string
	TrustedProxies     []netip.Prefix // VKDL_TRUSTED_PROXIES: faixas (CIDR) da borda que pode enviar o cabeçalho acima
	CFZoneID           string         // VKDL_CF_ZONE_ID: com CFAPIToken, purga o cache da Cloudflare ao apagar
	CFAPIToken         string         // VKDL_CF_API_TOKEN (permissão Cache Purge); nunca logar
	Storage            string         // VKDL_STORAGE: StorageFS ou StorageS3
	S3                 S3             // preenchido só quando Storage == StorageS3
}

// Backends de armazenamento aceitos em VKDL_STORAGE.
const (
	StorageFS = "fs"
	StorageS3 = "s3"
)

// S3 é a configuração do bucket (variáveis S3_*).
type S3 struct {
	Endpoint  string // host[:porta], sem esquema
	Region    string // vazio = descoberta automática
	Bucket    string
	KeyID     string // nunca logar
	KeySecret string // nunca logar
	Prefix    string
	UseSSL    bool
	PathStyle string // "auto", "true" ou "false"
}

// FromEnv monta a Config usando os.Getenv.
func FromEnv() (Config, error) {
	return Load(os.Getenv)
}

// Load monta a Config usando getenv (injetável para testes).
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Listen:             get("VKDL_LISTEN", ":8080"),
		BaseURL:            strings.TrimRight(get("VKDL_BASE_URL", "http://localhost:8080"), "/"),
		DataDir:            get("VKDL_DATA_DIR", "./data"),
		AdminToken:         strings.TrimSpace(getenv("VKDL_ADMIN_TOKEN")),
		TrustedProxyHeader: get("VKDL_TRUSTED_PROXY_HEADER", "CF-Connecting-IP"),
		CFZoneID:           strings.TrimSpace(getenv("VKDL_CF_ZONE_ID")),
		CFAPIToken:         strings.TrimSpace(getenv("VKDL_CF_API_TOKEN")),
	}
	if (c.CFZoneID == "") != (c.CFAPIToken == "") {
		return c, fmt.Errorf("VKDL_CF_ZONE_ID e VKDL_CF_API_TOKEN: defina os dois ou nenhum")
	}
	var err error
	if c.TrustedProxies, err = parsePrefixes(get("VKDL_TRUSTED_PROXIES", CloudflareRanges)); err != nil {
		return c, fmt.Errorf("VKDL_TRUSTED_PROXIES: %w", err)
	}
	if c.MaxStorage, err = ParseSize(get("VKDL_MAX_STORAGE", "10GB")); err != nil {
		return c, fmt.Errorf("VKDL_MAX_STORAGE: %w", err)
	}
	if c.MinFreeDisk, err = ParseSize(get("VKDL_MIN_FREE_DISK", "1GB")); err != nil {
		return c, fmt.Errorf("VKDL_MIN_FREE_DISK: %w", err)
	}
	if c.MaxUpload, err = ParseSize(get("VKDL_MAX_UPLOAD", "20MB")); err != nil {
		return c, fmt.Errorf("VKDL_MAX_UPLOAD: %w", err)
	}
	if c.MaxUpload <= 0 {
		return c, fmt.Errorf("VKDL_MAX_UPLOAD: precisa ser maior que zero")
	}
	if c.RateAnon, err = parseRate(get("VKDL_RATE_ANON", "30")); err != nil {
		return c, fmt.Errorf("VKDL_RATE_ANON: %w", err)
	}
	if c.RateKey, err = parseRate(get("VKDL_RATE_KEY", "300")); err != nil {
		return c, fmt.Errorf("VKDL_RATE_KEY: %w", err)
	}
	if err := loadStorage(&c, getenv); err != nil {
		return c, err
	}
	if s := getenv("VKDL_SECRET"); s != "" {
		c.Secret = []byte(s)
	} else {
		c.Secret = make([]byte, 32)
		rand.Read(c.Secret)
		c.SecretGenerated = true
	}
	return c, nil
}

// loadStorage escolhe o backend e, para S3, lê e valida as variáveis S3_*.
// Os erros citam o nome da variável, nunca o valor (pode ser credencial).
func loadStorage(c *Config, getenv func(string) string) error {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	c.Storage = strings.ToLower(env("VKDL_STORAGE"))
	switch c.Storage {
	case "":
		c.Storage = StorageFS
		if env("S3_BUCKET") != "" {
			c.Storage = StorageS3
		}
	case StorageFS, StorageS3:
	default:
		return fmt.Errorf("VKDL_STORAGE: valor inválido %q (use fs ou s3)", c.Storage)
	}
	if c.Storage != StorageS3 {
		return nil
	}
	s := S3{
		Region:    env("S3_REGION"),
		Bucket:    env("S3_BUCKET"),
		KeyID:     env("S3_KEY_ID"),
		KeySecret: env("S3_KEY_SECRET"),
		Prefix:    env("S3_PREFIX"),
		UseSSL:    true,
		PathStyle: strings.ToLower(env("S3_PATH_STYLE")),
	}
	for _, k := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_KEY_ID", "S3_KEY_SECRET"} {
		if env(k) == "" {
			return fmt.Errorf("%s: obrigatória com o armazenamento s3", k)
		}
	}
	if v := env("S3_USE_SSL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("S3_USE_SSL: use true ou false")
		}
		s.UseSSL = b
	}
	endpoint := env("S3_ENDPOINT")
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("S3_ENDPOINT: use host[:porta] ou http(s)://host[:porta]")
		}
		if u.User != nil {
			return fmt.Errorf("S3_ENDPOINT: não coloque credenciais na URL; use S3_KEY_ID e S3_KEY_SECRET")
		}
		if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("S3_ENDPOINT: não pode ter caminho; o bucket vai em S3_BUCKET e o prefixo em S3_PREFIX")
		}
		ssl := u.Scheme == "https"
		if env("S3_USE_SSL") != "" && s.UseSSL != ssl {
			return fmt.Errorf("S3_USE_SSL contradiz o esquema de S3_ENDPOINT")
		}
		s.UseSSL, endpoint = ssl, u.Host
	} else if strings.ContainsAny(endpoint, "/@?# ") {
		return fmt.Errorf("S3_ENDPOINT: use host[:porta] ou http(s)://host[:porta]")
	}
	s.Endpoint = endpoint
	switch s.PathStyle {
	case "":
		s.PathStyle = "auto"
	case "auto", "true", "false":
	default:
		return fmt.Errorf("S3_PATH_STYLE: use auto, true ou false")
	}
	c.S3 = s
	return nil
}

func parseRate(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("valor inválido %q (inteiro >= 0; 0 desativa o limite)", s)
	}
	return n, nil
}

// ParseSize interpreta tamanhos como "512", "20MB", "1.5GB" ou "2TiB" (base 1024).
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if t == "" {
		return 0, fmt.Errorf("tamanho vazio")
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(t, u.suffix) {
			t = strings.TrimSuffix(t, u.suffix)
			mult = u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("tamanho inválido %q", s)
	}
	return int64(n * mult), nil
}

// CloudflareRanges são as faixas publicadas em https://www.cloudflare.com/ips (conferidas em 2026-11-18).
// Só uma conexão vinda delas pode dizer quem é o cliente pelo CF-Connecting-IP.
const CloudflareRanges = "173.245.48.0/20,103.21.244.0/22,103.22.200.0/22,103.31.4.0/22,141.101.64.0/18," +
	"108.162.192.0/18,190.93.240.0/20,188.114.96.0/20,197.234.240.0/22,198.41.128.0/17,162.158.0.0/15," +
	"104.16.0.0/13,104.24.0.0/14,172.64.0.0/13,131.0.72.0/22," +
	"2400:cb00::/32,2606:4700::/32,2803:f800::/32,2405:b500::/32,2405:8100::/32,2a06:98c0::/29,2c0f:f248::/32"

// parsePrefixes lê uma lista de CIDRs separados por vírgula; "none" desliga a confiança no cabeçalho.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("CIDR inválido %q", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
