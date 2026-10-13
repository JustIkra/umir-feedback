package domain

import "strings"

type Namespace struct {
	Kind     string `json:"kind"`
	UserID   string `json:"userID,omitempty"`
	OrgID    string `json:"orgID,omitempty"`
	TenantID string `json:"tenantID,omitempty"`
}

func Anonymous() Namespace { return Namespace{Kind: "anonymous"} }
func (n Namespace) Valid() bool {
	if n.Kind == "anonymous" {
		return n.UserID == "" && n.OrgID == "" && n.TenantID == ""
	}
	return n.Kind == "app" && canonicalUUID(n.UserID) && canonicalUUID(n.OrgID) && canonicalUUID(n.TenantID)
}
func canonicalUUID(v string) bool { return v == strings.ToLower(v) && ValidID(SubmissionID(v)) }
func (n Namespace) Key(id SubmissionID) SubmissionID {
	if n.Kind == "anonymous" || n.Kind == "" {
		return id
	}
	return SubmissionID("app:" + n.UserID + ":" + n.OrgID + ":" + n.TenantID + ":" + string(id))
}
