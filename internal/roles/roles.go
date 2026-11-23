// Package roles is the single source of truth for AgentGate's
// role-based access control (backlog F26, tiered administrators
// with separation of duties).
//
// The model follows the "three powers" (三权分立) split required by
// China's classified-protection baseline (GB/T 22239-2019, level 3):
// the person who runs the system must not also write the security
// policy, and the person who audits must not be able to change
// anything. Mapped onto AgentGate:
//
//   - admin: platform administrator, full control. The only role
//     that may assign roles (UsersWrite).
//   - security: security administrator, same policy power as admin
//     (change classification policy, approve exceptions, export
//     audit) but may not assign roles.
//   - auditor: audit administrator, read-only over audit and
//     reports (query / verify / export / generate reports). May not
//     change any policy or configuration.
//   - operator: operations administrator, runs channels and tokens
//     (channel CRUD + keys, token limits, cache, OTel). May not read
//     audit or change policy.
//   - employee: ordinary user, no management surface.
//
// Key operations (change classification policy, approve an exception,
// export audit) require the matching role and are always audited;
// the audit entry records actor, target and the role before/after,
// never sensitive content.
package roles

// Role names. They are stored verbatim in store.User.Role.
const (
	Admin    = "admin"
	Security = "security"
	Auditor  = "auditor"
	Operator = "operator"
	Employee = "employee"
)

// All lists every role, most privileged first.
func All() []string { return []string{Admin, Security, Auditor, Operator, Employee} }

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool {
	switch r {
	case Admin, Security, Auditor, Operator, Employee:
		return true
	}
	return false
}

// MappableViaIdP reports whether a role may be granted through the
// OIDC group mapping (backlog F26). Only auditor / operator /
// employee: the IdP must never mint admin or security, and this is
// enforced here as well as in the mapping parser, so a miswired
// caller cannot escalate through the mapping either.
func MappableViaIdP(role string) bool {
	return role == Auditor || role == Operator || role == Employee
}

// IsAdminRole reports whether r carries full policy power: admin or
// security. This preserves the pre-F26 requireAdmin semantics
// (admin | security) for every policy endpoint.
func IsAdminRole(r string) bool { return r == Admin || r == Security }

// Permission is one management capability. The matrix below is the
// whole policy: anything not listed here is denied to everyone
// except through an explicit entry.
type Permission string

const (
	// AuditRead covers read-only audit surfaces: audit query and
	// chain verification, attestation reports, usage stats and
	// sidecar stats (all aggregates, no sensitive content).
	AuditRead Permission = "audit:read"
	// AuditExport covers taking audit data out of the platform:
	// audit NDJSON export and report CSV export.
	AuditExport Permission = "audit:export"
	// ReportsWrite covers generating a compliance report (producing
	// a frozen audit artifact, not changing policy).
	ReportsWrite Permission = "reports:write"
	// ReportsDelete covers deleting a generated report.
	ReportsDelete Permission = "reports:delete"
	// AuditArchive covers moving audit events into a cold archive.
	AuditArchive Permission = "audit:archive"
	// Channels covers channel CRUD and channel-key management.
	Channels Permission = "channels"
	// Tokens covers listing tokens and changing their rate limits.
	Tokens Permission = "tokens"
	// Cache covers the response-cache switch and clearing.
	Cache Permission = "cache"
	// OTel covers the OpenTelemetry status and connectivity test.
	OTel Permission = "otel"
	// Policy covers every other configuration change: skills /
	// prompts / knowledge packages review and publish, folder
	// sources, semantic regeneration, attestation policies, package
	// distribution, org sync, budgets, alert webhooks, exception
	// approval / rejection / revocation, OIDC status.
	Policy Permission = "policy"
	// UsersRead covers listing users (no password hashes).
	UsersRead Permission = "users:read"
	// UsersWrite covers changing a user's role, department or
	// disabled flag. Admin only: even the security administrator
	// cannot grant roles, so no one can promote themselves.
	UsersWrite Permission = "users:write"
)

// matrix grants each permission to a set of roles.
var matrix = map[Permission]map[string]bool{
	AuditRead:     {Admin: true, Security: true, Auditor: true},
	AuditExport:   {Admin: true, Security: true, Auditor: true},
	ReportsWrite:  {Admin: true, Security: true, Auditor: true},
	ReportsDelete: {Admin: true, Security: true},
	AuditArchive:  {Admin: true, Security: true},
	Channels:      {Admin: true, Security: true, Operator: true},
	Tokens:        {Admin: true, Security: true, Operator: true},
	Cache:         {Admin: true, Security: true, Operator: true},
	OTel:          {Admin: true, Security: true, Operator: true},
	Policy:        {Admin: true, Security: true},
	UsersRead:     {Admin: true, Security: true},
	UsersWrite:    {Admin: true},
}

// Can reports whether role holds permission p. Unknown roles and
// unknown permissions are denied.
func Can(role string, p Permission) bool {
	allowed, ok := matrix[p]
	return ok && allowed[role]
}

// Description is the human-readable role card used by the management
// console and docs/roles.md.
type Description struct {
	Role      string `json:"role"`
	NameZH    string `json:"name_zh"`
	NameEN    string `json:"name_en"`
	SummaryZH string `json:"summary_zh"`
	SummaryEN string `json:"summary_en"`
}

var descriptions = []Description{
	{Admin, "平台管理员", "Platform admin", "全部权限；唯一可以分配角色的角色。", "Full control; the only role that may assign roles."},
	{Security, "安全管理员", "Security admin", "与平台管理员同等的策略管理权：改分级策略、批例外、导审计；不可分配角色。", "Same policy power as the platform admin (change policy, approve exceptions, export audit); may not assign roles."},
	{Auditor, "审计员", "Auditor", "只读审计与报表：查审计、验链、导审计、生成与导出报表；不可改任何策略。", "Read-only over audit and reports; may not change any policy."},
	{Operator, "运维员", "Operator", "渠道与令牌运维：渠道增删改、密钥、令牌限流、缓存、OTel；不可看审计。", "Channel and token operations; may not read audit."},
	{Employee, "员工", "Employee", "普通员工：使用 Agent 能力，无管理面。", "Ordinary user: uses agent capabilities, no management surface."},
}

// Describe returns the description card for a role, or false when
// the role is unknown.
func Describe(role string) (Description, bool) {
	for _, d := range descriptions {
		if d.Role == role {
			return d, true
		}
	}
	return Description{}, false
}

// Descriptions returns the description card for every role.
func Descriptions() []Description {
	out := make([]Description, len(descriptions))
	copy(out, descriptions)
	return out
}
