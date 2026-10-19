package main

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// The method→privilege table behind the call() gate: an embedded copy of
// contracts/method-privileges.json from the app repo, because this repo
// must build standalone and cannot read contracts/ at build time. `just
// sync-specs` refreshes it and `check-specs-synced` gates drift — and
// drift fails closed anyway: a method missing here is refused, and the
// actuator's own per-method privilege check never depends on this copy.
//
//go:embed method_privileges.json
var methodPrivilegesRaw []byte

var methodPrivilegesOnce struct {
	sync.Once
	table map[string]string // method -> privilege name; "" = privilege-less
}

// methodPrivilege resolves a platform method to its privilege name.
// ok=false means the method isn't in the table at all — bridgeCall
// refuses those. A privilege-less method (JSON null) comes back as "".
func methodPrivilege(method string) (priv string, ok bool) {
	methodPrivilegesOnce.Do(func() {
		var doc struct {
			Methods map[string]*string `json:"methods"`
		}
		methodPrivilegesOnce.table = map[string]string{}
		if json.Unmarshal(methodPrivilegesRaw, &doc) != nil {
			return // empty table: every call refused, never wrongly allowed
		}
		for m, p := range doc.Methods {
			if p == nil {
				methodPrivilegesOnce.table[m] = ""
			} else {
				methodPrivilegesOnce.table[m] = *p
			}
		}
	})
	priv, ok = methodPrivilegesOnce.table[method]
	return priv, ok
}
