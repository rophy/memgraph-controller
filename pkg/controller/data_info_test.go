package controller

import (
	"testing"
)

func TestParseDataInfo_HealthyASYNC(t *testing.T) {
	input := `{memgraph: {behind: 0, status: "ready", ts: 2}}`
	
	result, err := parseDataInfo(input)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Behind != 0 {
		t.Errorf("Expected Behind=0, got %d", result.Behind)
	}
	
	if result.Status != "ready" {
		t.Errorf("Expected Status=ready, got %s", result.Status)
	}
	
	if result.Timestamp != 2 {
		t.Errorf("Expected Timestamp=2, got %d", result.Timestamp)
	}
	
	if !result.IsHealthy {
		t.Errorf("Expected IsHealthy=true, got %v", result.IsHealthy)
	}
	
	if result.ErrorReason != "" {
		t.Errorf("Expected empty ErrorReason for healthy replica, got: %s", result.ErrorReason)
	}
}

func TestParseDataInfo_UnhealthyASYNC(t *testing.T) {
	input := `{memgraph: {behind: -20, status: "invalid", ts: 0}}`
	
	result, err := parseDataInfo(input)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Behind != -20 {
		t.Errorf("Expected Behind=-20, got %d", result.Behind)
	}
	
	if result.Status != "invalid" {
		t.Errorf("Expected Status=invalid, got %s", result.Status)
	}
	
	if result.Timestamp != 0 {
		t.Errorf("Expected Timestamp=0, got %d", result.Timestamp)
	}
	
	if result.IsHealthy {
		t.Errorf("Expected IsHealthy=false, got %v", result.IsHealthy)
	}
	
	if result.ErrorReason == "" {
		t.Error("Expected non-empty ErrorReason for unhealthy replica")
	}
}

func TestParseDataInfo_EmptyObject(t *testing.T) {
	input := `{}`
	
	result, err := parseDataInfo(input)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Status != "empty" {
		t.Errorf("Expected Status=empty, got %s", result.Status)
	}
	
	if result.Behind != 0 {
		t.Errorf("Expected Behind=0 for empty object, got %d", result.Behind)
	}
	
	if result.IsHealthy {
		t.Errorf("Expected IsHealthy=false for empty data_info, got %v", result.IsHealthy)
	}
	
	expectedReason := "Empty data_info - possible connection failure"
	if result.ErrorReason != expectedReason {
		t.Errorf("Expected ErrorReason=%s, got %s", expectedReason, result.ErrorReason)
	}
}

func TestParseDataInfo_EmptyString(t *testing.T) {
	input := ""
	
	result, err := parseDataInfo(input)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Status != "unknown" {
		t.Errorf("Expected Status=unknown, got %s", result.Status)
	}
	
	if result.Behind != -1 {
		t.Errorf("Expected Behind=-1 for missing data_info, got %d", result.Behind)
	}
	
	if result.IsHealthy {
		t.Errorf("Expected IsHealthy=false for missing data_info, got %v", result.IsHealthy)
	}
	
	expectedReason := "Missing data_info field"
	if result.ErrorReason != expectedReason {
		t.Errorf("Expected ErrorReason=%s, got %s", expectedReason, result.ErrorReason)
	}
}

func TestParseDataInfo_MalformedJSON(t *testing.T) {
	input := `{invalid json structure}`
	
	result, err := parseDataInfo(input)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Status != "malformed" {
		t.Errorf("Expected Status=malformed, got %s", result.Status)
	}
	
	if result.Behind != -1 {
		t.Errorf("Expected Behind=-1 for malformed data_info, got %d", result.Behind)
	}
	
	if result.IsHealthy {
		t.Errorf("Expected IsHealthy=false for malformed data_info, got %v", result.IsHealthy)
	}
	
	if result.ErrorReason == "" {
		t.Error("Expected non-empty ErrorReason for malformed data_info")
	}
}

func TestParseDataInfo_WithWhitespace(t *testing.T) {
	input := `  {memgraph: {behind: 5, status: "ready", ts: 10}}  `
	
	result, err := parseDataInfo(input)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Behind != 5 {
		t.Errorf("Expected Behind=5, got %d", result.Behind)
	}
	
	if result.Status != "ready" {
		t.Errorf("Expected Status=ready, got %s", result.Status)
	}
	
	if !result.IsHealthy {
		t.Errorf("Expected IsHealthy=true, got %v", result.IsHealthy)
	}
}

func TestAssessReplicationHealth(t *testing.T) {
	tests := []struct {
		name          string
		status        string
		behind        int
		expectHealthy bool
		expectReason  string
	}{
		{
			name:          "healthy ready state",
			status:        "ready",
			behind:        0,
			expectHealthy: true,
			expectReason:  "",
		},
		{
			name:          "healthy ready with small lag",
			status:        "ready", 
			behind:        5,
			expectHealthy: true,
			expectReason:  "",
		},
		{
			name:          "invalid status",
			status:        "invalid",
			behind:        0,
			expectHealthy: false,
			expectReason:  "Replication status marked as invalid",
		},
		{
			name:          "negative lag",
			status:        "ready",
			behind:        -5,
			expectHealthy: false,
			expectReason:  "Negative replication lag detected: -5",
		},
		{
			name:          "empty status",
			status:        "empty",
			behind:        0,
			expectHealthy: false,
			expectReason:  "Empty data_info indicates connection failure",
		},
		{
			name:          "unknown status",
			status:        "unknown",
			behind:        0,
			expectHealthy: false,
			expectReason:  "Unable to determine replication health",
		},
		{
			name:          "custom unknown status",
			status:        "connecting",
			behind:        0,
			expectHealthy: false,
			expectReason:  "Unknown replication status: connecting",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthy, reason := assessReplicationHealth(tt.status, tt.behind)
			
			if healthy != tt.expectHealthy {
				t.Errorf("Expected healthy=%v, got %v", tt.expectHealthy, healthy)
			}
			
			if reason != tt.expectReason {
				t.Errorf("Expected reason=%q, got %q", tt.expectReason, reason)
			}
		})
	}
}

func TestReplicaInfo_HealthMethods(t *testing.T) {
	// Test with healthy replica
	healthyReplica := &ReplicaInfo{
		Name: "test-replica-1",
		ParsedDataInfo: &DataInfoStatus{
			Behind:      0,
			Status:      "ready",
			IsHealthy:   true,
			ErrorReason: "",
		},
	}
	
	if !healthyReplica.IsHealthy() {
		t.Error("Expected healthy replica to return true for IsHealthy()")
	}
	
	if healthyReplica.GetHealthReason() != "Replica is healthy" {
		t.Errorf("Expected 'Replica is healthy', got %s", healthyReplica.GetHealthReason())
	}
	
	if healthyReplica.RequiresRecovery() {
		t.Error("Expected healthy replica to not require recovery")
	}
	
	if healthyReplica.GetRecoveryAction() != "none" {
		t.Errorf("Expected recovery action 'none', got %s", healthyReplica.GetRecoveryAction())
	}
	
	// Test with unhealthy replica
	unhealthyReplica := &ReplicaInfo{
		Name: "test-replica-2", 
		ParsedDataInfo: &DataInfoStatus{
			Behind:      -20,
			Status:      "invalid",
			IsHealthy:   false,
			ErrorReason: "Replication status marked as invalid",
		},
	}
	
	if unhealthyReplica.IsHealthy() {
		t.Error("Expected unhealthy replica to return false for IsHealthy()")
	}
	
	if unhealthyReplica.GetHealthReason() != "Replication status marked as invalid" {
		t.Errorf("Expected error reason, got %s", unhealthyReplica.GetHealthReason())
	}
	
	if !unhealthyReplica.RequiresRecovery() {
		t.Error("Expected unhealthy replica to require recovery")
	}
	
	if unhealthyReplica.GetRecoveryAction() != "re-register" {
		t.Errorf("Expected recovery action 're-register', got %s", unhealthyReplica.GetRecoveryAction())
	}
	
	// Test with nil ParsedDataInfo
	nilReplica := &ReplicaInfo{
		Name:           "test-replica-3",
		ParsedDataInfo: nil,
	}
	
	if nilReplica.IsHealthy() {
		t.Error("Expected replica with nil ParsedDataInfo to return false for IsHealthy()")
	}
	
	if nilReplica.GetHealthReason() != "No health information available" {
		t.Errorf("Expected 'No health information available', got %s", nilReplica.GetHealthReason())
	}
	
	if nilReplica.RequiresRecovery() {
		t.Error("Expected replica with nil ParsedDataInfo to not require recovery")
	}
	
	if nilReplica.GetRecoveryAction() != "investigate" {
		t.Errorf("Expected recovery action 'investigate', got %s", nilReplica.GetRecoveryAction())
	}
}

func TestReplicaInfo_GetRecoveryAction_Variations(t *testing.T) {
	tests := []struct {
		name           string
		status         string
		behind         int
		expectedAction string
	}{
		{
			name:           "invalid status should re-register",
			status:         "invalid",
			behind:         0,
			expectedAction: "re-register",
		},
		{
			name:           "empty status should restart pod",
			status:         "empty",
			behind:         0,
			expectedAction: "restart-pod",
		},
		{
			name:           "parse error should investigate",
			status:         "parse_error", 
			behind:         -1,
			expectedAction: "investigate",
		},
		{
			name:           "severe lag should require manual intervention",
			status:         "ready",
			behind:         -15,
			expectedAction: "manual-intervention",
		},
		{
			name:           "moderate lag should re-register",
			status:         "ready",
			behind:         -5,
			expectedAction: "re-register",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replica := &ReplicaInfo{
				Name: "test-replica",
				ParsedDataInfo: &DataInfoStatus{
					Behind:      tt.behind,
					Status:      tt.status,
					IsHealthy:   false,
					ErrorReason: "test error",
				},
			}
			
			action := replica.GetRecoveryAction()
			if action != tt.expectedAction {
				t.Errorf("Expected recovery action %s, got %s", tt.expectedAction, action)
			}
		})
	}
}