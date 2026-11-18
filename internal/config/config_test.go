package config

import "testing"

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
	if c.MaxStorage != 10<<30 || c.MinFreeDisk != 1<<30 || c.MaxUpload != 20<<20 {
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
