package audit

import (
	"bytes"
	"strings"
	"testing"
	"encoding/json"

	"github.com/hashicorp/vault/logical"
	"errors"
)

func TestFormatJSON_formatRequest(t *testing.T) {
	cases := map[string]struct {
		Auth   *logical.Auth
		Req    *logical.Request
		Err    error
		Result string
	}{
		"auth, request": {
			&logical.Auth{ClientToken: "foo", Policies: []string{"root"}},
			&logical.Request{
				Operation: logical.WriteOperation,
				Path:      "/foo",
				Connection: &logical.Connection{
					RemoteAddr: "127.0.0.1",
				},
			},
			errors.New("this is an error"),
			testFormatJSONReqBasicStr,
		},
	}

	for name, tc := range cases {
		var buf bytes.Buffer
		var format FormatJSON
		if err := format.FormatRequest(&buf, tc.Auth, tc.Req, tc.Err); err != nil {
			t.Fatalf("bad: %s\nerr: %s", name, err)
		}

		var expectedjson = new(JSONRequestEntry)
		if err := json.Unmarshal([]byte(tc.Result), &expectedjson); err != nil {
			t.Fatalf("bad json: %s", err)
		}

		var actualjson = new(JSONRequestEntry)
		if err := json.Unmarshal([]byte(buf.String()), &actualjson); err != nil {
			t.Fatalf("bad json: %s", err)
		}

		expectedjson.Time = actualjson.Time

		expectedBytes, err := json.Marshal(expectedjson)
		if err != nil {
			t.Fatalf("unable to marshal json: %s", err)
		}

		if strings.TrimSpace(buf.String()) != string(expectedBytes) {
			t.Fatalf(
				"bad: %s\nResult:\n\n'%s'\n\nExpected:\n\n'%s'",
				name, buf.String(), string(expectedBytes))
		}
	}
}

const testFormatJSONReqBasicStr = `{"time":"2015-08-05T13:45:46Z","type":"request","auth":{"display_name":"","policies":["root"],"metadata":null},"request":{"operation":"write","path":"/foo","data":null,"remote_address":"127.0.0.1"},"error":"this is an error"}
`
