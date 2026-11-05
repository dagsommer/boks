package secret

import (
	"encoding/json"
	"testing"
	"time"
)

// TestCredentialFileTemplateSeesScopesJSON: the kit grammar's .ScopesJSON renders as a JSON
// array, and as [] rather than null when the credential has no scopes.
func TestCredentialFileTemplateSeesScopesJSON(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	record.FileTemplate = `{"scopes":{{.ScopesJSON}}}`
	credential, err := record.Credential()
	if err != nil {
		t.Fatal(err)
	}
	files, err := CredentialFiles([]Credential{credential})
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %v, err = %v", files, err)
	}
	var doc struct{ Scopes []string }
	if err := json.Unmarshal(files[0].Content, &doc); err != nil || len(doc.Scopes) != len(record.Scopes) {
		t.Errorf("rendered %q: %v", files[0].Content, err)
	}

	record.Scopes = nil
	credential, _ = record.Credential()
	files, _ = CredentialFiles([]Credential{credential})
	if string(files[0].Content) != `{"scopes":[]}` {
		t.Errorf("no scopes rendered %q", files[0].Content)
	}
}
