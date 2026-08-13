package service

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

var (
	ErrPassportUnavailable = errors.New("变更通行证签名服务未配置")
	ErrPassportInvalid     = errors.New("变更通行证无效")
	ErrPassportExpired     = errors.New("变更通行证已过期")
	ErrPassportRevoked     = errors.New("变更通行证已撤销或已消费")
	ErrPassportReplay      = errors.New("变更通行证已被消费，拒绝重放")
	ErrArtifactMismatch    = errors.New("制品摘要与变更通行证不匹配")
	ErrEnvironmentMismatch = errors.New("目标环境与变更通行证不匹配")
	ErrRuleSetChanged      = errors.New("风险规则版本已变化，请重新检查和审批")
)

func passportSignerFromEnvironment() (*changegate.Signer, time.Duration) {
	signer, _ := changegate.NewSigner(os.Getenv("DBGUARD_PASSPORT_HMAC_SECRET"))
	ttl := 10 * time.Minute
	if configured, err := time.ParseDuration(strings.TrimSpace(os.Getenv("DBGUARD_PASSPORT_TTL"))); err == nil && configured >= time.Minute && configured <= 30*time.Minute {
		ttl = configured
	}
	return signer, ttl
}

func (s *Service) PassportConfigured() bool { return s.passportSigner != nil }

func (s *Service) PassportsForChange(changeID, actorID string) ([]model.Passport, error) {
	change, err := s.ChangeFor(changeID, actorID)
	if err != nil {
		return nil, err
	}
	return s.store.PassportsByChange(change.OrganizationID, change.ID), nil
}

func (s *Service) IssuePassport(changeID, actorID string, ttlSeconds int) (model.PassportCredential, error) {
	if s.passportSigner == nil {
		return model.PassportCredential{}, ErrPassportUnavailable
	}
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.PassportCredential{}, ErrForbidden
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.PassportCredential{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.PassportCredential{}, ErrForbidden
	}
	if actor.ID == change.SubmitterID || change.ReviewerID == "" || actor.ID != change.ReviewerID {
		return model.PassportCredential{}, fmt.Errorf("%w：只有实际审批人可以签发通行证", ErrForbidden)
	}
	if change.Status != model.StatusApproved || change.CheckRun == nil || change.CheckRun.Status != "PASSED" || change.CheckRun.Blocking != 0 {
		return model.PassportCredential{}, fmt.Errorf("%w：变更尚未完成确定性检查与独立审批", ErrInvalidState)
	}
	if change.ArtifactSHA256 == "" || change.RuleSetVersion == "" || change.CheckRun.ArtifactSHA256 != change.ArtifactSHA256 || change.CheckRun.RuleSetVersion != change.RuleSetVersion {
		return model.PassportCredential{}, ErrPassportInvalid
	}
	currentRules := changegate.RuleSetVersion(s.store.PoliciesByOrganization(change.OrganizationID))
	if currentRules != change.RuleSetVersion {
		return model.PassportCredential{}, ErrRuleSetChanged
	}
	if change.Experiment != nil && (strings.EqualFold(change.Experiment.Mode, "DEMO_ONLY") || strings.EqualFold(change.Experiment.Status, "NOT_RUN")) {
		return model.PassportCredential{}, fmt.Errorf("%w：演示或未运行的验证结果不能签发通行证", ErrInvalidState)
	}
	if strings.TrimSpace(change.SQL) != "" && !trustedSQLExperiment(change.Experiment, change) {
		return model.PassportCredential{}, fmt.Errorf("%w：SQL 变更必须通过真实 PostgreSQL 影子演练", ErrInvalidState)
	}

	ttl := s.passportTTL
	if ttlSeconds > 0 {
		requested := time.Duration(ttlSeconds) * time.Second
		if requested < time.Minute || requested > 30*time.Minute {
			return model.PassportCredential{}, fmt.Errorf("%w：通行证有效期必须在 60 到 1800 秒之间", ErrValidation)
		}
		ttl = requested
	}
	passportID, err := changegate.NewPassportID()
	if err != nil {
		return model.PassportCredential{}, ErrPassportUnavailable
	}
	now := time.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(ttl)
	claims := changegate.PassportClaims{
		PassportID: passportID, OrganizationID: change.OrganizationID, ChangeID: change.ID,
		ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, RuleSetVersion: change.RuleSetVersion,
		ApproverID: change.ReviewerID, IssuedAtUnix: now.Unix(), ExpiresAtUnix: expiresAt.Unix(),
	}
	token, err := s.passportSigner.Sign(claims)
	if err != nil {
		return model.PassportCredential{}, ErrPassportUnavailable
	}
	passport := model.Passport{
		ID: passportID, OrganizationID: change.OrganizationID, ChangeID: change.ID,
		ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, RuleSetVersion: change.RuleSetVersion,
		ApproverID: change.ReviewerID, ApproverName: change.ReviewerName, Status: model.PassportActive,
		TokenSHA256: changegate.TokenSHA256(token), IssuedAt: now, ExpiresAt: expiresAt,
	}
	issueAudit := auditPassport(audit(actor, change.ID, "PASSPORT_ISSUED", fmt.Sprintf("签发短时一次性通行证 %s，绑定制品 %s、环境 %s 和规则版本 %s", passport.ID, passport.ArtifactSHA256, passport.Environment, passport.RuleSetVersion)), passport.ID)
	issueAudit.RequestDigest = change.ArtifactSHA256
	if err := s.store.CreatePassport(passport, issueAudit); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return model.PassportCredential{}, fmt.Errorf("%w：该变更已有有效通行证，请先消费、过期或撤销", ErrInvalidState)
		}
		return model.PassportCredential{}, err
	}
	passport.TokenSHA256 = ""
	return model.PassportCredential{Passport: passport, Token: token}, nil
}

func (s *Service) VerifyGate(input model.GateRequest, consume bool) (model.GateResult, error) {
	if s.passportSigner == nil {
		return model.GateResult{}, ErrPassportUnavailable
	}
	token := strings.TrimSpace(input.Token)
	claims, err := s.passportSigner.Verify(token)
	if err != nil {
		return model.GateResult{}, ErrPassportInvalid
	}
	if !strings.EqualFold(strings.TrimSpace(input.ArtifactSHA256), claims.ArtifactSHA256) {
		return model.GateResult{}, ErrArtifactMismatch
	}
	if !strings.EqualFold(strings.TrimSpace(input.Environment), strings.TrimSpace(claims.Environment)) {
		return model.GateResult{}, ErrEnvironmentMismatch
	}
	passport, err := s.store.Passport(claims.PassportID)
	if err != nil {
		return model.GateResult{}, ErrPassportInvalid
	}
	switch passport.Status {
	case model.PassportConsumed:
		return model.GateResult{}, ErrPassportReplay
	case model.PassportRevoked:
		return model.GateResult{}, ErrPassportRevoked
	case model.PassportExpired:
		return model.GateResult{}, ErrPassportExpired
	}
	if passport.OrganizationID != claims.OrganizationID || passport.ChangeID != claims.ChangeID || passport.ArtifactSHA256 != claims.ArtifactSHA256 || passport.Environment != claims.Environment || passport.RuleSetVersion != claims.RuleSetVersion || passport.ApproverID != claims.ApproverID {
		return model.GateResult{}, ErrPassportInvalid
	}
	issuedAt, expiresAt := changegate.ClaimsTimes(claims)
	if !passport.IssuedAt.Equal(issuedAt) || !passport.ExpiresAt.Equal(expiresAt) {
		return model.GateResult{}, ErrPassportInvalid
	}
	change, err := s.store.Change(passport.ChangeID)
	if err != nil || change.OrganizationID != passport.OrganizationID || change.Status != model.StatusApproved || change.ArtifactSHA256 != passport.ArtifactSHA256 || change.Environment != passport.Environment || change.RuleSetVersion != passport.RuleSetVersion {
		return model.GateResult{}, ErrPassportInvalid
	}
	if changegate.RuleSetVersion(s.store.PoliciesByOrganization(change.OrganizationID)) != passport.RuleSetVersion {
		return model.GateResult{}, ErrRuleSetChanged
	}
	consumer := strings.TrimSpace(input.Consumer)
	if consumer == "" {
		consumer = "ci-gate"
	}
	if len(consumer) > 200 {
		return model.GateResult{}, fmt.Errorf("%w：consumer 不能超过 200 个字符", ErrValidation)
	}
	now := time.Now().UTC()
	action := "PASSPORT_VERIFIED"
	if consume {
		action = "PASSPORT_CONSUMED"
	}
	gateActor := model.User{ID: "ci:" + consumer, OrganizationID: passport.OrganizationID, Name: consumer, Role: "CI", Active: true}
	gateAudit := auditPassport(audit(gateActor, passport.ChangeID, action, fmt.Sprintf("CI 校验通行证 %s，制品与环境匹配", passport.ID)), passport.ID)
	gateAudit.RequestDigest = input.ArtifactSHA256
	validated, err := s.store.UsePassport(passport.ID, changegate.TokenSHA256(token), consumer, now, consume, gateAudit)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPassportExpired):
			return model.GateResult{}, ErrPassportExpired
		case errors.Is(err, store.ErrPassportInactive):
			if consume {
				return model.GateResult{}, ErrPassportReplay
			}
			return model.GateResult{}, ErrPassportRevoked
		case errors.Is(err, store.ErrPassportTokenMismatch), errors.Is(err, store.ErrPassportChangeInvalid):
			return model.GateResult{}, ErrPassportInvalid
		default:
			return model.GateResult{}, err
		}
	}
	if !consume {
		verifyAudit := auditPassport(audit(gateActor, passport.ChangeID, action, fmt.Sprintf("CI 验签通过：%s", passport.ID)), passport.ID)
		verifyAudit.RequestDigest = input.ArtifactSHA256
		_ = s.store.RecordAudit(verifyAudit)
	} else if completed, changeErr := s.store.Change(passport.ChangeID); changeErr == nil {
		s.publish(completed, "CI 通行证已消费，生产变更自动完成")
	}
	return model.GateResult{Allowed: true, Code: "GATE_ALLOWED", Reason: "制品摘要、环境、规则版本、审批人和有效期均匹配", Passport: &validated}, nil
}

func (s *Service) RevokePassport(changeID, passportID, actorID string) (model.Passport, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.Passport{}, ErrForbidden
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.Passport{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") || actor.ID == change.SubmitterID {
		return model.Passport{}, ErrForbidden
	}
	passport, err := s.store.Passport(passportID)
	if err != nil || passport.ChangeID != change.ID {
		return model.Passport{}, store.ErrNotFound
	}
	return s.store.RevokePassport(change.OrganizationID, passportID, actor.ID, time.Now().UTC(), audit(actor, change.ID, "PASSPORT_REVOKED", "人工撤销变更通行证 "+passportID))
}

func trustedSQLExperiment(report *model.ExperimentReport, change model.ChangeRequest) bool {
	return report != nil && strings.EqualFold(report.Mode, "POSTGRES") && strings.EqualFold(report.Status, "PASSED") && report.RollbackVerified &&
		report.ArtifactSHA256 == change.ArtifactSHA256 && report.RuleSetVersion == change.RuleSetVersion
}
