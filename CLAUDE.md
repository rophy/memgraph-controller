Notes for running up a memgraph cluster under community edition.

# IMPORTANT

1. For long running tasks such as `make run`, NEVER run as foreground command, NEVER as background command using '&'. ALWAYS run as background process using Claude run_in_background parameter.
2. After completing your tests, clean up the background processes.

# Development Workflow

## Running Unit Tests

```bash
make test
```

## Running E2E Tests

**In-Cluster E2E Testing (Recommended):**

1. Run `make down` to remove skaffold resources. If kubectl context failed to connect to a kubernetes cluster, FAIL IMMEDIATELY and prompt human to fix kubectl context.
2. Run `make run` at background, which should buils and deploy a memgraph-ha cluster with skaffold.
3. Wait for the memgraph-ha cluster to stablize. Docker build takes around 120s, other parts around 30s.
   Can check memgraph-controller pod logs to assist.
4. Run `make test-e2e`, which builds E2E test container and runs tests as Kubernetes Job.
5. Run `make test-e2e-cleanup` to clean up test resources.

**Local E2E Testing (Legacy):**
- Use `make test-e2e-local` for local testing (requires manual port-forward setup)
- Note: Local testing may not honor readiness probes and could connect to non-leader pods

**E2E Test Architecture:**
- Tests run as Kubernetes Jobs in the `memgraph` namespace
- Connect directly to `memgraph-controller` service (honors readiness probes)
- Only connects to leader pods, testing real production behavior

## Standard Development Process

**Any code change must go through these steps**

1. Run unit tests. If not all tests passed, pause and confirm with huamn whether claude should fix unit tests first.
2. Implement new feature.
4. Run staticcheck and make sure no errors.
3. Update unit tests,which should be in same folder as source code. Do NOT add unit tests to tests/ folder which is for e2e tests.
4. Run unit tests, make sure all tests pass.
5. Run e2e tests.


## Change Management Protocol

When issues are found during log analysis, I will:

1. **Describe the Fix First**: Clearly explain what needs to be changed and why
2. **Ask for Confirmation**: Wait for your explicit approval before making changes
3. **Implement Only After Approval**: Make the changes only after you confirm

This ensures you stay informed about all modifications and maintains control over the codebase evolution.

## Debugging Memgraph Replication

**For debugging replication issues, always query Memgraph directly using mgconsole in the pods:**

```bash
# Check replication role of a pod
kubectl exec <pod-name> -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'

# Check registered replicas from main pod
kubectl exec <main-pod-name> -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'

# Check storage info
kubectl exec <pod-name> -- bash -c 'echo "SHOW STORAGE INFO;" | mgconsole --output-format csv --username=memgraph'
```

**Do NOT rely on the memgraph-controller status API for debugging** - always verify the actual Memgraph state directly using the above commands.

# DESIGN COMPLIANCE FRAMEWORK

> **CRITICAL**: All code MUST implement specific parts of [DESIGN.md](./DESIGN.md)  
> **CRITICAL**: All code MUST NOT contradict any part of [DESIGN.md](./DESIGN.md)  
> **CRITICAL**: Before making code changes, identify which design section is being implemented  
> **CRITICAL**: All code changes MUST comply with [DESIGN.md](./DESIGN.md)

## Mandatory Design Compliance Process

### Before ANY Code Changes

**STEP 1: Read Design Section**
```bash
# ALWAYS identify which DESIGN.md section you're implementing
# Example: "Implementing DESIGN.md section 'Actions for Reconciliation' steps 1-3"
```

**STEP 2: Quote Exact Requirements** 
```
DESIGN.md says: "[exact quote from design document]"
My code will implement: "[specific implementation approach]"
```

**STEP 3: Check for Contradictions**
- MUST verify implementation doesn't contradict ANY part of DESIGN.md
- If ANY contradiction found: STOP and request human review
- NO exceptions - design consistency is mandatory

### Implementation Rules

#### ✅ ALLOWED Code Patterns
- Code that directly implements a specific DESIGN.md section
- Code that references which design requirement it fulfills  
- Simple implementations that follow design steps exactly

#### ❌ FORBIDDEN Code Patterns
- "Discovery-based" logic not specified in design
- Complex algorithms not mentioned in DESIGN.md
- "Smart" logic that tries to handle edge cases beyond design scope
- Any code that contradicts or works around design specifications

### Examples

#### ✅ CORRECT Implementation
```go
// Implements DESIGN.md "Actions for Reconciliation" Step 2
func (c *Controller) showReplicas(ctx context.Context, mainPod string) error {
    // Run `SHOW REPLICAS` to main pod to check replication status
    return c.memgraphClient.QueryReplicasWithRetry(ctx, mainPodAddress)
}
```

#### ❌ INCORRECT Implementation  
```go
// FORBIDDEN: Discovery-based logic not in DESIGN.md
func (c *Controller) findCurrentMain(clusterState *ClusterState) string {
    // Try to discover which pod is currently main...
    for podName, podInfo := range clusterState.Pods {
        if podInfo.MemgraphRole == "main" {
            return podName  // CONTRADICTION: Design uses controller authority, not discovery
        }
    }
}
```

## Contradiction Detection Protocol

### When Code Contradicts Design

1. **IMMEDIATE STOP**: Halt implementation 
2. **FLAG FOR HUMAN**: Report exact contradiction found
3. **DESIGN REVIEW**: Work with human to resolve design vs implementation conflict
4. **UPDATE DESIGN FIRST**: Fix DESIGN.md before continuing with code

### Design Update Process

1. **Human Approval**: All DESIGN.md changes require human review
2. **Consistency Check**: Verify change doesn't break other design sections  
3. **Code Update**: Only implement after design is updated and approved

## Design Compliance Rules

### For Implementation

1. **Reference Requirement**: Every function/method MUST reference which design section it implements
2. **No Contradiction**: Code MUST NOT implement logic that contradicts this design
3. **Complete Coverage**: All design sections marked as "IMPLEMENTATION REQUIREMENT" MUST have corresponding code
4. **Single Source**: DESIGN.md is the authoritative design source - README.md is user documentation only

### For Modifications

1. **Design First**: Design changes MUST be approved in DESIGN.md before code implementation
2. **Consistency Check**: Any design change MUST be verified against all existing sections for conflicts
3. **Human Review**: Design contradictions MUST be resolved with human review before proceeding

### For Code Review

1. **Design Mapping**: Every code change MUST identify which design section is being implemented
2. **Contradiction Detection**: Any code that might contradict design MUST be flagged for human review
3. **Coverage Verification**: All "IMPLEMENTATION REQUIREMENT" sections MUST have test coverage

## Quality Gates

### Pre-Commit Checklist

- [ ] Code implements specific DESIGN.md section (documented in code)
- [ ] No contradictions with ANY part of DESIGN.md
- [ ] Design section quoted in commit message
- [ ] Implementation approach explained and justified

### Code Review Focus Areas

1. **Design Mapping**: Which exact DESIGN.md section does this implement?
2. **Contradiction Check**: Does this contradict any design principle?
3. **Simplicity**: Is this the simplest possible implementation of the design?
4. **Completeness**: Are ALL design requirements for this section implemented?

## Emergency Procedures

### If You Catch Claude Violating Design

**STOP IMMEDIATELY** and say:
> "DESIGN VIOLATION: This contradicts DESIGN.md section [X]. Please quote the exact design requirement and explain how your code implements it."

### If Design Seems Wrong or Incomplete

**DO NOT work around it in code**. Instead:
> "DESIGN ISSUE: The design doesn't cover [scenario]. Should we update DESIGN.md to specify the correct behavior?"

# Known Issues Documentation

**All known issues are documented in `KNOWN_ISSUES.md`**

## Protocol for Claude

1. **Before investigating new issues**: ALWAYS read `KNOWN_ISSUES.md` first to check if the problem is already documented
2. **When encountering test failures or bugs**: Check against documented known issues to avoid duplicate investigation
3. **When documenting new issues**: Add them to `KNOWN_ISSUES.md` with:
   - Clear description and reproduction steps
   - Root cause analysis
   - Evidence (logs, error messages)
   - Proposed solutions
   - Impact assessment
4. **When fixing issues**: Update the status in `KNOWN_ISSUES.md` and reference the fix commit

This ensures we maintain a comprehensive knowledge base of system behavior and avoid repeatedly investigating the same problems.
