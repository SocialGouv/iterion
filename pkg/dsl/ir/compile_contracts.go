package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime"
	"regexp"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

const (
	DiagRuntimeSemantics   DiagCode = "C300"
	DiagPublicContract     DiagCode = "C301"
	DiagPublicPort         DiagCode = "C302"
	DiagPortPolicy         DiagCode = "C303"
	DiagPortReference      DiagCode = "C304"
	DiagPortSupplier       DiagCode = "C305"
	DiagPortCompatibility  DiagCode = "C306"
	DiagPortCycle          DiagCode = "C307"
	DiagPortMapAxes        DiagCode = "C308"
	DiagPortControl        DiagCode = "C309"
	DiagPublicCriterion    DiagCode = "C310"
	DiagPortExport         DiagCode = "C311"
	DiagPortHiddenInput    DiagCode = "C312"
	DiagPortImplementation DiagCode = "C313"
)

var publicNamePattern = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*$`)

func validPublicName(name string) bool { return publicNamePattern.MatchString(name) }

func portSource(span ast.Span) PortSource {
	return PortSource{File: span.Start.File, Line: span.Start.Line, Column: span.Start.Column}
}

func (c *compiler) contractError(code DiagCode, span ast.Span, format string, args ...any) {
	c.emit(SeverityError, code, "", "", span, "", format, args...)
}

func publicDigest(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "" // malformed JSON parameters already have a located diagnostic
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *compiler) compilePublicDeclarations() (map[string]*PublicContract, map[string]*PortPolicy) {
	contracts := map[string]*PublicContract{}
	for _, decl := range c.file.Contracts {
		if decl == nil {
			c.errorf(DiagPublicContract, "missing public contract declaration")
			continue
		}
		if !validPublicName(decl.Name) || contracts[decl.Name] != nil {
			c.contractError(DiagPublicContract, decl.Span, "invalid or duplicate contract name %q", decl.Name)
			continue
		}
		contract := &PublicContract{
			Name: decl.Name, DisplayName: decl.DisplayName, Responsibility: decl.Responsibility,
			Version: decl.Version, Source: portSource(decl.Span),
		}
		if strings.TrimSpace(decl.DisplayName) == "" || strings.TrimSpace(decl.Responsibility) == "" {
			c.contractError(DiagPublicContract, decl.Span, "contract %q requires display_name and responsibility", decl.Name)
		}
		if contract.Version == 0 {
			contract.Version = 1
		}
		if contract.Version < 1 {
			c.contractError(DiagPublicContract, decl.Span, "contract %q version must be positive", decl.Name)
		}
		contract.Inputs = c.compilePublicPorts(decl.Inputs, true)
		contract.Outputs = c.compilePublicPorts(decl.Outputs, false)
		seen := map[string]bool{}
		for _, rule := range decl.Criteria {
			if rule == nil {
				c.contractError(DiagPublicCriterion, decl.Span, "contract %q contains a missing criterion", decl.Name)
				continue
			}
			if !validPublicName(rule.Name) || seen[rule.Name] {
				c.contractError(DiagPublicCriterion, rule.Span, "invalid or duplicate criterion name %q", rule.Name)
			}
			seen[rule.Name] = true
			endpoint, err := ParsePortEndpoint(rule.Port)
			var port *PublicPort
			if err == nil {
				switch endpoint.Node {
				case "input":
					port = FindPublicPort(contract.Inputs, endpoint.Port)
				case "output":
					port = FindPublicPort(contract.Outputs, endpoint.Port)
				}
			}
			if port == nil {
				c.contractError(DiagPublicCriterion, rule.Span, "criterion %q references unknown public port %q", rule.Name, rule.Port)
			}
			definition, known := spec.LookupPublicCriterion(rule.Kind)
			if _, err := spec.CompilePublicCriterion(rule.Kind, rule.Params); err != nil {
				c.contractError(DiagPublicCriterion, rule.Span, "%v", err)
			}
			if port != nil && known {
				typeName := port.Type.Name
				if port.Type.ArrayDepth > 0 {
					typeName = "array"
				}
				if !slices.Contains(definition.Types, typeName) {
					c.contractError(DiagPublicCriterion, rule.Span, "criterion %q does not accept type %s", rule.Kind, port.Type)
				}
			}
			contract.Criteria = append(contract.Criteria, PublicCriterion{
				Name: rule.Name, Kind: rule.Kind, Port: rule.Port,
				Params: append(json.RawMessage(nil), rule.Params...), Source: portSource(rule.Span),
			})
		}
		seen = map[string]bool{}
		for _, effect := range decl.Effects {
			if effect == nil {
				c.contractError(DiagPublicContract, decl.Span, "contract %q contains a missing effect", decl.Name)
				continue
			}
			if !validPublicName(effect.Name) || seen[effect.Name] || strings.TrimSpace(effect.Description) == "" {
				c.contractError(DiagPublicContract, effect.Span, "effect %q needs a unique name and a description", effect.Name)
			}
			seen[effect.Name] = true
			contract.Effects = append(contract.Effects, PublicEffect{
				Name: effect.Name, Description: effect.Description, Paid: effect.Paid, Source: portSource(effect.Span),
			})
		}
		contract.Identity = publicContractDigest(contract)
		contracts[decl.Name] = contract
	}
	policies := map[string]*PortPolicy{}
	for _, decl := range c.file.PortPolicies {
		if decl == nil {
			c.errorf(DiagPortPolicy, "missing port policy declaration")
			continue
		}
		if !validPublicName(decl.Name) || policies[decl.Name] != nil {
			c.contractError(DiagPortPolicy, decl.Span, "invalid or duplicate port_policy name %q", decl.Name)
			continue
		}
		policy := &PortPolicy{Name: decl.Name, MaxMapItems: decl.MaxMapItems}
		if decl.MaxMapItems < 0 {
			c.contractError(DiagPortPolicy, decl.Span, "max_map_items must be non-negative; zero inherits the configured limit")
		}
		seen := map[string]bool{}
		for _, effect := range decl.Effects {
			if effect == nil {
				c.contractError(DiagPortPolicy, decl.Span, "port_policy %q contains a missing effect", decl.Name)
				continue
			}
			if !validPublicName(effect.Name) || seen[effect.Name] {
				c.contractError(DiagPortPolicy, effect.Span, "invalid or duplicate effect policy %q", effect.Name)
			}
			seen[effect.Name] = true
			switch effect.Recovery {
			case "idempotent", "manual":
				if effect.Verifier != "" {
					c.contractError(DiagPortPolicy, effect.Span, "verifier requires recovery: verify")
				}
			case "verify":
				if strings.TrimSpace(effect.Verifier) == "" {
					c.contractError(DiagPortPolicy, effect.Span, "recovery: verify requires a verifier")
				}
			default:
				c.contractError(DiagPortPolicy, effect.Span, "unknown effect recovery %q; use idempotent, verify or manual", effect.Recovery)
			}
			policy.Effects = append(policy.Effects, PortEffectPolicy{
				Name: effect.Name, Resource: effect.Resource, Recovery: effect.Recovery, Verifier: effect.Verifier,
			})
		}
		policy.Identity = publicDigest(policy)
		policies[decl.Name] = policy
	}
	return contracts, policies
}

func (c *compiler) compilePublicPorts(decls []*ast.PortDecl, inputs bool) []PublicPort {
	ports := make([]PublicPort, 0, len(decls))
	seen := map[string]bool{}
	for _, decl := range decls {
		if decl == nil {
			c.errorf(DiagPublicPort, "missing public port declaration")
			continue
		}
		if !validPublicName(decl.Name) || strings.HasPrefix(decl.Name, "_") || seen[decl.Name] {
			c.contractError(DiagPublicPort, decl.Span, "port %q must have a unique name without a reserved '_' prefix", decl.Name)
		}
		seen[decl.Name] = true
		t, err := ResolvePortType(decl.Type, c.schemas)
		if err != nil {
			c.contractError(DiagPublicPort, decl.Span, "%v", err)
		}
		port := PublicPort{
			Name: decl.Name, Type: t, Description: decl.Description, Required: decl.IsRequired(),
			Nullable: decl.Nullable, Default: append(json.RawMessage(nil), decl.Default...),
			MinItems: copyPortInt(decl.MinItems), MaxItems: copyPortInt(decl.MaxItems), Source: portSource(decl.Span),
		}
		if decl.MinItems != nil || decl.MaxItems != nil {
			if t.ArrayDepth == 0 {
				c.contractError(DiagPublicPort, decl.Span, "port %q cardinality constraints require an array", decl.Name)
			}
			if (decl.MinItems != nil && *decl.MinItems < 0) || (decl.MaxItems != nil && *decl.MaxItems < 0) ||
				(decl.MinItems != nil && decl.MaxItems != nil && *decl.MinItems > *decl.MaxItems) {
				c.contractError(DiagPublicPort, decl.Span, "port %q has inconsistent min_items/max_items", decl.Name)
			}
		}
		if file := decl.File; file != nil {
			if t.Name != "file" || t.Schema != nil {
				c.contractError(DiagPublicPort, file.Span, "file properties require a file-valued port")
			}
			port.File = &PortFile{MediaType: file.MediaType, MinBytes: file.MinBytes}
			if file.MinBytes < 0 {
				c.contractError(DiagPublicPort, file.Span, "min_bytes must be non-negative")
			}
			if file.MediaType != "" {
				if _, _, err := mime.ParseMediaType(file.MediaType); err != nil {
					c.contractError(DiagPublicPort, file.Span, "invalid media_type %q", file.MediaType)
				}
			}
			if file.Schema != "" {
				schemaType, err := ResolvePortType(file.Schema, c.schemas)
				if err != nil || schemaType.Schema == nil || schemaType.ArrayDepth != 0 {
					c.contractError(DiagPublicPort, file.Span, "file schema %q must resolve to a declared object schema", file.Schema)
				} else {
					port.File.Schema, port.File.ShapeHash = schemaType.Schema, schemaType.ShapeHash
				}
			}
		}
		if decl.Default != nil {
			if !inputs || port.Required {
				c.contractError(DiagPublicPort, decl.Span, "default is allowed only on optional input ports")
			}
			value, err := DecodePortValue(decl.Default)
			if err == nil {
				err = port.ValidateValue(value)
			}
			if err != nil {
				c.contractError(DiagPublicPort, decl.Span, "invalid default for port %q: %v", decl.Name, err)
			}
		}
		ports = append(ports, port)
	}
	return ports
}

func copyPortInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// A formatting change relocates declarations but does not change a contract.
// Complete source/implementation identity is tracked separately by the run.
func publicContractDigest(contract *PublicContract) string {
	cp := *contract
	cp.Identity, cp.Source = "", PortSource{}
	clearSources := func(ports []PublicPort) []PublicPort {
		out := append([]PublicPort(nil), ports...)
		for i := range out {
			out[i].Source = PortSource{}
		}
		return out
	}
	cp.Inputs, cp.Outputs = clearSources(cp.Inputs), clearSources(cp.Outputs)
	cp.Criteria = append([]PublicCriterion(nil), cp.Criteria...)
	for i := range cp.Criteria {
		cp.Criteria[i].Source = PortSource{}
	}
	cp.Effects = append([]PublicEffect(nil), cp.Effects...)
	for i := range cp.Effects {
		cp.Effects[i].Source = PortSource{}
	}
	return publicDigest(cp)
}
