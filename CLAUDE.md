Notes for running up a memgraph cluster under community edition.

Reference: https://memgraph.com/docs/clustering/replication

- The following error in replica nodes can safely be ignored.

```
[memgraph_log] [error] Handling SystemRecovery, an enterprise RPC message, without license. Check your license status by running SHOW LICENSE INFO.
```

# Development Workflow

## Standard Development Process

Our development workflow follows this pattern:

1. **Claude Implements**: I implement the requested feature or fix
2. **User Runs Skaffold**: You run `skaffold run` or `skaffold dev` to deploy and test
3. **Claude Checks Logs**: I check the deployment logs to verify functionality and identify issues

## Change Management Protocol

When issues are found during log analysis, I will:

1. **Describe the Fix First**: Clearly explain what needs to be changed and why
2. **Ask for Confirmation**: Wait for your explicit approval before making changes
3. **Implement Only After Approval**: Make the changes only after you confirm

This ensures you stay informed about all modifications and maintains control over the codebase evolution.

