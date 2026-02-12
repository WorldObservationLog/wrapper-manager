package main

import (
	"os"
	"testing"
)

func setupStorefrontFile() (func(), error) {
	const filename = "data/storefront_ids.json"
	originalContent, err := os.ReadFile(filename)
	originalExists := err == nil

	content := `[{"name":"United States","code":"us","storefrontId":143441},{"name":"China","code":"cn","storefrontId":143465}]`
	_ = os.MkdirAll("data", 0755)
	_ = os.WriteFile(filename, []byte(content), 0644)

	teardown := func() {
		os.Remove(filename)
		if originalExists {
			os.WriteFile(filename, originalContent, 0644)
		}
	}
	return teardown, nil
}

func TestParseStorefrontID(t *testing.T) {
	teardown, _ := setupStorefrontFile()
	defer teardown()

	// Verify known ID
	code := parseStorefrontID("143441-1,29")
	if code != "us" {
		t.Errorf("Expected 'us', got '%s'", code)
	}

	// Verify another known ID
	code = parseStorefrontID("143465-something")
	if code != "cn" {
		t.Errorf("Expected 'cn', got '%s'", code)
	}
}

func BenchmarkParseStorefrontID(b *testing.B) {
	teardown, _ := setupStorefrontFile()
	defer teardown()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = parseStorefrontID("143441-1,29")
	}
}
