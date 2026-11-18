package v1beta1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGCPNodeServiceAccountSerializationCompatibility(t *testing.T) {
	// The previous worker field used the controller email type. Changing its
	// schema must preserve the same JSON string representation in both directions.
	type previous struct {
		Email  GCPServiceAccountEmail `json:"email,omitempty"`
		Scopes []string               `json:"scopes,omitempty"`
	}
	for _, test := range []struct{ name, email string }{
		{name: "When the worker identity is omitted, it should preserve field omission"},
		{name: "When a custom worker identity is set, it should preserve the previous JSON representation", email: "worker@my-project-123.iam.gserviceaccount.com"},
		{name: "When a default Compute identity is set, it should round trip through previous Go clients", email: "35809355916-compute@developer.gserviceaccount.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			email := test.email
			current := GCPNodeServiceAccount{Email: GCPNodeServiceAccountEmail(email)}
			old := previous{Email: GCPServiceAccountEmail(email)}
			if email != "" {
				current.Scopes = []string{"https://www.googleapis.com/auth/cloud-platform"}
				old.Scopes = append([]string(nil), current.Scopes...)
			}
			data, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			var decodedOld previous
			if err := json.Unmarshal(data, &decodedOld); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(old, decodedOld) {
				t.Fatal("old client cannot read current worker identity")
			}
			data, err = json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			var decodedCurrent GCPNodeServiceAccount
			if err := json.Unmarshal(data, &decodedCurrent); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current, decodedCurrent) {
				t.Fatal("current client cannot read previous worker identity")
			}
		})
	}
}
