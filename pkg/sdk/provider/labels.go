package provider

// Reserved label keys (ADR-013, plan R2). These are the ONLY names any
// Fleetplane component may use for ownership/identity labels; drivers apply
// them verbatim from DesiredState.Labels and parse them in Discover results.
const (
	// LabelManaged marks a resource as Fleetplane-managed. Value: "true".
	LabelManaged = "fleetplane.io/managed"
	// LabelOwner carries the control-plane instance identity (OwnerID),
	// separating two Fleetplanes sharing one cloud project.
	LabelOwner = "fleetplane.io/owner"
	// LabelID carries the Fleetplane resource ID (res_...).
	LabelID = "fleetplane.io/id"
	// LabelOp carries the create operation ID (op_...) — the create-dedup
	// anchor for crash resolution (ActionID := OperationID, plan R2).
	LabelOp = "fleetplane.io/op"

	// LabelClass records the class a resource was created from, so
	// re-adopted resources regain their reclaim policy (plan R9).
	LabelClass = "fleetplane.io/class"

	// LabelTest and LabelTestRun mark E2E resources; sweepers refuse to
	// touch anything without LabelTest (ADR-015).
	LabelTest    = "fleetplane.io/test"
	LabelTestRun = "fleetplane.io/test-run"
)

// IdentityLabels composes the ownership/identity label set the kernel bakes
// into every create (via DesiredState.Labels).
func IdentityLabels(ownerID, resourceID, opID string) map[string]string {
	return map[string]string{
		LabelManaged: "true",
		LabelOwner:   ownerID,
		LabelID:      resourceID,
		LabelOp:      opID,
	}
}
