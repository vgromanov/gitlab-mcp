// Package readmeta defines shared read-section completeness types and codes.
package readmeta

// Stable limitation/error codes for this foundation slice (closed set).
const (
	CodeInaccessible       = "inaccessible"
	CodeUnsupported        = "unsupported"
	CodePartial            = "partial"
	CodeInconsistent       = "inconsistent"
	CodeCollapsed          = "collapsed"
	CodeTooLarge           = "too_large"
	CodeBudgetItems        = "budget_items"
	CodeBudgetBytes        = "budget_bytes"
	CodeBudgetElapsed      = "budget_elapsed"
	CodeBudgetRequests     = "budget_requests"
	CodeHTTPError          = "http_error"
	CodeCancelled          = "cancelled"
	CodeUnknownCount       = "unknown_count"
	CodeAuthzDenied        = "authz_denied"
	CodeIdentityUnresolved = "identity_unresolved"
)

// AllCodes is the closed initial code set for tests and docs.
var AllCodes = []string{
	CodeInaccessible,
	CodeUnsupported,
	CodePartial,
	CodeInconsistent,
	CodeCollapsed,
	CodeTooLarge,
	CodeBudgetItems,
	CodeBudgetBytes,
	CodeBudgetElapsed,
	CodeBudgetRequests,
	CodeHTTPError,
	CodeCancelled,
	CodeUnknownCount,
	CodeAuthzDenied,
	CodeIdentityUnresolved,
}
