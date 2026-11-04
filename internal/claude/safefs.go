package claude

import "github.com/moophat/agent-quota-exporter/internal/safefs"

// The no-link file access moved to internal/safefs in v0.2 so other plugins share it; these names keep the claude
// package and its tests unchanged.
var (
	fsReason    = safefs.Reason
	openDir     = safefs.OpenDir
	openSubdir  = safefs.OpenSubdir
	statAt      = safefs.StatAt
	realDir     = safefs.RealDir
	openRegular = safefs.OpenRegular
)
