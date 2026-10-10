package permission

// Evaluate 仅执行规则判断，不读取或修改授权，不执行工具，不进行 I/O。
// 授权只能解决 Ask；调用方应在执行前用当前 Policy 重新判断。
func Evaluate(policy Policy, request Request) Result {
	result := Result{RequestID: request.RequestID, SessionID: request.SessionID}
	decide := func(decision Decision, reason Reason) Result {
		result.Decision, result.Reason = decision, reason
		return result
	}
	if !validRequest(request) {
		return decide(Deny, InvalidRequest)
	}
	for _, tool := range policy.DeniedTools {
		if tool == request.Tool {
			return decide(Deny, ExplicitDeny)
		}
	}
	if (policy.WorkMode != Plan && policy.WorkMode != Build) ||
		(policy.ApprovalMode != Default && policy.ApprovalMode != AutoEdit && policy.ApprovalMode != AutoApprove) {
		return decide(Deny, InvalidPolicy)
	}
	if policy.WorkMode == Plan {
		if request.Tool.Source == Builtin {
			if request.Capability == ReadOnly {
				return decide(Allow, BuiltinReadOnly)
			}
			if request.Capability == FileWrite && allTargetsWithin(policy, request.FileTargets, policy.PlanDir, true) {
				return decide(Allow, PlanMarkdown)
			}
		}
		return decide(Deny, PlanRestriction)
	}
	if request.Tool.Source == External {
		return decide(Ask, ExternalConfirmation)
	}
	if request.Capability == ReadOnly {
		return decide(Allow, BuiltinReadOnly)
	}
	if request.Capability == Orchestration {
		return decide(Allow, BuiltinOrchestration)
	}
	if policy.ApprovalMode == AutoApprove {
		return decide(Allow, BuiltinAutoApprove)
	}
	switch request.Capability {
	case FileWrite:
		if policy.ApprovalMode == AutoEdit && allTargetsWithin(policy, request.FileTargets, policy.WorkingDir, false) {
			return decide(Allow, WorkspaceAutoEdit)
		}
		return decide(Ask, WriteConfirmation)
	case Shell:
		return decide(Ask, ShellConfirmation)
	default:
		return decide(Ask, UnknownConfirmation)
	}
}

func validRequest(request Request) bool {
	return request.RequestID != "" && request.SessionID != "" &&
		request.Tool.SourceID != "" && request.Tool.ToolID != "" &&
		(request.Tool.Source == Builtin || request.Tool.Source == External) &&
		(request.Capability == ReadOnly || request.Capability == FileWrite ||
			request.Capability == Shell || request.Capability == Orchestration || request.Capability == Unknown)
}
