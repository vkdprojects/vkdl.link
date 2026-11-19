package config

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":      0,
		"512":    512,
		"1KB":    1024,
		"20MB":   20 << 20,
		"20mb":   20 << 20,
		"10GB":   10 << 30,
		"1.5GB":  3 << 29,
		"2TiB":   2 << 40,
		"100 MB": 100 << 20,
		"64k":    64 << 10,
		"7B":     7,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1MB", "MB", "1XB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) devia falhar", bad)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.BaseURL != "http://localhost:8080" || c.DataDir != "./data" {
		t.Errorf("padrões errados: %+v", c)
	}
	if c.MaxStorage != 10<<30 || c.MinFreeDisk != 1<<30 || c.MaxUpload != 20<<20 || c.PendingMax != 512<<20 {
		t.Errorf("tamanhos padrão errados: %+v", c)
	}
	if c.RateAnon != 30 || c.RateKey != 300 || c.TrustedProxyHeader != "CF-Connecting-IP" {
		t.Errorf("limites padrão errados: %+v", c)
	}
	if !c.SecretGenerated || len(c.Secret) != 32 || c.AdminToken != "" {
		t.Errorf("segredo/admin padrão errados: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := map[string]string{
		"VKDL_BASE_URL":    "https://vkdl.link/",
		"VKDL_MAX_UPLOAD":  "5MB",
		"VKDL_SECRET":      "s3cr3t",
		"VKDL_ADMIN_TOKEN": " adm ",
		"VKDL_RATE_ANON":   "0",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://vkdl.link" || c.MaxUpload != 5<<20 || string(c.Secret) != "s3cr3t" ||
		c.SecretGenerated || c.AdminToken != "adm" || c.RateAnon != 0 {
		t.Errorf("overrides não aplicados: %+v", c)
	}
	for k, v := range map[string]string{"VKDL_MAX_UPLOAD": "0", "VKDL_RATE_KEY": "-3", "VKDL_MAX_STORAGE": "x"} {
		if _, err := Load(func(key string) string {
			if key == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s devia falhar", k, v)
		}
	}
}

func TestLoadCloudflarePurge(t *testing.T) {
	env := map[string]string{"VKDL_CF_ZONE_ID": "zona", "VKDL_CF_API_TOKEN": "tok"}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil || c.CFZoneID != "zona" || c.CFAPIToken != "tok" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	delete(env, "VKDL_CF_API_TOKEN")
	if _, err := Load(func(k string) string { return env[k] }); err == nil {
		t.Fatal("só a zona sem o token devia falhar")
	}
}

func loadMap(env map[string]string) (Config, error) {
	return Load(func(k string) string { return env[k] })
}

func s3Env() map[string]string {
	return map[string]string{
		"S3_ENDPOINT":   "s3.us-east-1.idrivee2.com",
		"S3_REGION":     "us-east-1",
		"S3_BUCKET":     "vkdl",
		"S3_KEY_ID":     "ID-NAO-PODE-VAZAR",
		"S3_KEY_SECRET": "SEGREDO-NAO-PODE-VAZAR",
	}
}

func TestStorageSelection(t *testing.T) {
	c, err := loadMap(nil)
	if err != nil || c.Storage != StorageFS {
		t.Fatalf("sem S3_BUCKET o padrão é fs: %q, %v", c.Storage, err)
	}
	env := s3Env()
	c, err = loadMap(env)
	if err != nil || c.Storage != StorageS3 {
		t.Fatalf("com S3_BUCKET o padrão é s3: %q, %v", c.Storage, err)
	}
	want := S3{Endpoint: "s3.us-east-1.idrivee2.com", Region: "us-east-1", Bucket: "vkdl",
		KeyID: "ID-NAO-PODE-VAZAR", KeySecret: "SEGREDO-NAO-PODE-VAZAR", UseSSL: true, PathStyle: "auto"}
	if c.S3 != want {
		t.Fatalf("S3 = %+v", c.S3)
	}
	env["VKDL_STORAGE"] = "FS"
	if c, err = loadMap(env); err != nil || c.Storage != StorageFS || c.S3 != (S3{}) {
		t.Fatalf("VKDL_STORAGE=fs com S3_* definidas: %q %+v, %v", c.Storage, c.S3, err)
	}
	if _, err := loadMap(map[string]string{"VKDL_STORAGE": "gcs"}); err == nil {
		t.Fatal("VKDL_STORAGE desconhecido devia falhar")
	}
}

func TestS3MissingVarsNameTheVariable(t *testing.T) {
	for _, k := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_KEY_ID", "S3_KEY_SECRET"} {
		env := s3Env()
		env["VKDL_STORAGE"] = "s3"
		delete(env, k)
		_, err := loadMap(env)
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("sem %s: erro %v devia citar a variável", k, err)
			continue
		}
		if strings.Contains(err.Error(), "NAO-PODE-VAZAR") {
			t.Errorf("sem %s: erro vazou credencial: %v", k, err)
		}
	}
	// região é opcional (descoberta automática)
	env := s3Env()
	delete(env, "S3_REGION")
	if c, err := loadMap(env); err != nil || c.S3.Region != "" {
		t.Fatalf("sem S3_REGION: %+v, %v", c.S3, err)
	}
}

func TestS3EndpointAndOptions(t *testing.T) {
	cases := []struct {
		endpoint, ssl, style string
		wantHost             string
		wantSSL              bool
		wantStyle            string
	}{
		{"minio:9000", "false", "true", "minio:9000", false, "true"},
		{"http://minio:9000", "", "", "minio:9000", false, "auto"},
		{"https://s3.example.com/", "", "FALSE", "s3.example.com", true, "false"},
		{"http://garage:3900", "false", "auto", "garage:3900", false, "auto"},
		{"s3.example.com", "", "", "s3.example.com", true, "auto"},
	}
	for _, tc := range cases {
		env := s3Env()
		env["S3_ENDPOINT"], env["S3_USE_SSL"], env["S3_PATH_STYLE"] = tc.endpoint, tc.ssl, tc.style
		c, err := loadMap(env)
		if err != nil {
			t.Errorf("%q: %v", tc.endpoint, err)
			continue
		}
		if c.S3.Endpoint != tc.wantHost || c.S3.UseSSL != tc.wantSSL || c.S3.PathStyle != tc.wantStyle {
			t.Errorf("%q: %+v", tc.endpoint, c.S3)
		}
	}
	bad := []map[string]string{
		{"S3_ENDPOINT": "ftp://host"},
		{"S3_ENDPOINT": "https://host/bucket"},
		{"S3_ENDPOINT": "https://ID-NAO-PODE-VAZAR:SEGREDO-NAO-PODE-VAZAR@host"},
		{"S3_ENDPOINT": "host/bucket"},
		{"S3_ENDPOINT": "https://host", "S3_USE_SSL": "false"},
		{"S3_USE_SSL": "talvez"},
		{"S3_PATH_STYLE": "virtual"},
	}
	for _, over := range bad {
		env := s3Env()
		for k, v := range over {
			env[k] = v
		}
		_, err := loadMap(env)
		if err == nil {
			t.Errorf("%v devia falhar", over)
			continue
		}
		if strings.Contains(err.Error(), "NAO-PODE-VAZAR") {
			t.Errorf("%v: erro vazou credencial: %v", over, err)
		}
	}
}

func TestS3Prefix(t *testing.T) {
	env := s3Env()
	env["S3_PREFIX"] = " vkdl/prod/ "
	c, err := loadMap(env)
	if err != nil || c.S3.Prefix != "vkdl/prod/" {
		t.Fatalf("prefixo = %q, %v", c.S3.Prefix, err)
	}
}
