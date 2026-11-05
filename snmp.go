package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// SNMP v2c, nur lesend, Community public: Auswertung z. B. durch ein Monitoring.
// v1/v2c beantwortet der Agent ohne eigene Einstellung, solange die Community
// keine Anmeldung verlangt (security=none).

const snmpCommunity = "public"

func planSNMP(ctx context.Context, cl *routeros.Client, on bool) (Plan, error) {
	cfg, err := printAll(ctx, cl, "/snmp/print")
	if err != nil {
		return Plan{Action: "snmp"}, err
	}
	if len(cfg) == 0 {
		return Plan{Action: "snmp", Blocked: "Das Gerät liefert keine SNMP-Einstellungen."}, nil
	}
	comms, err := printAll(ctx, cl, "/snmp/community/print", "=.proplist=name,read-access,write-access,security")
	if err != nil {
		return Plan{Action: "snmp"}, err
	}
	return buildSNMPPlan(cfg[0]["enabled"] == "true", comms, on), nil
}

func buildSNMPPlan(enabled bool, comms []map[string]string, on bool) Plan {
	p := Plan{Action: "snmp"}
	if !on {
		if !enabled {
			p.Noop = true
			p.Steps = []string{"SNMP ist bereits aus – nichts zu tun"}
			return p
		}
		p.Steps = []string{"SNMP ausschalten (Community bleibt eingetragen)"}
		p.Apply, p.Undo = "/snmp set enabled=no\n", try("/snmp set enabled=yes")
		return p
	}

	var c map[string]string
	for _, m := range comms {
		if m["name"] == snmpCommunity {
			c = m
		}
	}
	var apply, undo strings.Builder
	find := "[find name=" + q(snmpCommunity) + "]"
	switch {
	case c == nil:
		p.Steps = append(p.Steps, "Community public anlegen (nur lesend)")
		apply.WriteString("/snmp community add name=" + q(snmpCommunity) + " read-access=yes write-access=no security=none\n")
		undo.WriteString(try("/snmp community remove " + find))
	case c["read-access"] != "true" || c["write-access"] == "true" || (c["security"] != "" && c["security"] != "none"):
		p.Steps = append(p.Steps, "Community public auf „nur lesend, ohne Anmeldung“ setzen")
		apply.WriteString("/snmp community set " + find + " read-access=yes write-access=no security=none\n")
		cmd := fmt.Sprintf("/snmp community set %s read-access=%s write-access=%s", find, rosBool(c["read-access"]), rosBool(c["write-access"]))
		if c["security"] != "" {
			cmd += " security=" + c["security"]
		}
		undo.WriteString(try(cmd))
	}
	if !enabled {
		p.Steps = append(p.Steps, "SNMP einschalten")
		apply.WriteString("/snmp set enabled=yes\n")
		undo.WriteString(try("/snmp set enabled=no"))
	}
	if len(p.Steps) == 0 {
		p.Noop = true
		p.Steps = []string{"SNMP ist bereits an, Community public nur lesend – nichts zu tun"}
		return p
	}
	p.Warn = []string{"Community public ist kein Geheimnis: Wer das Gerät per UDP 161 erreicht, kann Gerätedaten lesen. Eine Firewall auf dem Gerät kann SNMP sperren oder beschränken; der Manager ändert sie nicht."}
	p.Apply, p.Undo = apply.String(), undo.String()
	return p
}

func rosBool(v string) string {
	if v == "true" {
		return "yes"
	}
	return "no"
}
