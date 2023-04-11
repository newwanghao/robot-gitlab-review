package main

import (
	"encoding/base64"
	"fmt"
	"github.com/opensourceways/community-robot-lib/gitlabclient"
	"github.com/xanzy/go-gitlab"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"
)

const (
	msgPRConflicts        = "PR conflicts to the target branch."
	msgMissingLabels      = "PR does not have these lables: %s"
	msgInvalidLabels      = "PR should remove these labels: %s"
	msgNotEnoughLGTMLabel = "PR needs %d lgtm labels and now gets %d"
	msgFrozenWithOwner    = "The target branch of PR has been frozen and it can be merge only by branch owners: %s"
	//legalLabelsAddedBy    = "openeuler-ci-bot"
	legalLabelsAddedBy = "wanghao"
	canMergeStatus     = "can_be_merged"
	ActionAddLabel     = "add"
)

var regCheckPr = regexp.MustCompile(`(?mi)^/check-pr\s*$`)

func (bot *robot) handleCheckPR(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != gitlabclient.ActionOpened ||
		e.ObjectKind != "note" ||
		!regCheckPr.MatchString(gitlabclient.GetMRCommentBody(e)) {
		return nil
	}

	return bot.tryMerge(e, cfg, true, log)
}

func (bot *robot) tryMerge(e *gitlab.MergeCommentEvent, cfg *botConfig, addComment bool, log *logrus.Entry) error {
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenter := gitlabclient.GetMRCommentAuthor(e)
	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)

	mergeRequest, err := bot.cli.GetMergeRequest(pid, number)
	if err != nil {
		return err
	}

	mergeMethod := bot.genMergeMethod(&mergeRequest, org, repo, log)

	h := mergeHelper{
		cfg:     cfg,
		pid:     pid,
		mrID:    number,
		org:     org,
		repo:    repo,
		method:  mergeMethod,
		author:  mergeRequest.Author.Username,
		cli:     bot.cli,
		mr:      &mergeRequest,
		trigger: gitlabclient.GetMRCommentAuthor(e),
	}

	if r, ok := h.canMerge(log); !ok {
		if len(r) > 0 && addComment {
			return bot.cli.CreateMergeRequestComment(
				pid, number,
				fmt.Sprintf(
					"@%s , this pr is not mergeable and the reasons are below:\n%s",
					commenter, strings.Join(r, "\n"),
				),
			)
		}

		return nil
	}

	return h.merge()
}

func (bot *robot) handleLabelUpdate(e *gitlab.MergeEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.ObjectAttributes.Action != "update" && !gitlabclient.CheckLabelUpdate(e) {
		return nil
	}
	org, repo := gitlabclient.GetMROrgAndRepo(e)

	mergeRequest, err := bot.cli.GetMergeRequest(e.Project.ID, e.ObjectAttributes.IID)
	if err != nil {
		return err
	}

	mergeMethod := bot.genMergeMethod(&mergeRequest, org, repo, log)

	h := mergeHelper{
		cfg:    cfg,
		org:    org,
		repo:   repo,
		method: mergeMethod,
		author: mergeRequest.Author.Username,
		cli:    bot.cli,
		mr:     &mergeRequest,
	}

	if _, ok := h.canMerge(log); ok {
		return h.merge()
	}

	return nil
}

type mergeHelper struct {
	mr  *gitlab.MergeRequest
	cfg *botConfig

	pid     int
	mrID    int
	org     string
	repo    string
	method  string
	author  string
	trigger string

	cli iClient
}

func (m *mergeHelper) merge() error {

	desc := m.genMergeDesc()

	opts := gitlab.UpdateMergeRequestOptions{Description: &desc, AssigneeIDs: &[]int{}, ReviewerIDs: &[]int{}}
	_, err := m.cli.UpdateMergeRequest(m.pid, m.mrID, opts)
	if err != nil {
		return err
	}

	return m.cli.MergeMergeRequest(
		m.pid, m.mrID,
	)
}

func (m *mergeHelper) canMerge(log *logrus.Entry) ([]string, bool) {
	if m.mr.MergeStatus != canMergeStatus {
		return []string{msgPRConflicts}, false
	}

	ops, err := m.cli.GetMergeRequestLabelChanges(m.pid, m.mrID)
	if err != nil {
		return []string{}, false
	}

	if r := isLabelMatched(m.getMRLabels(), m.cfg, ops, log); len(r) > 0 {
		return r, false
	}

	freeze, err := m.getFreezeInfo(log)
	if err != nil {
		return nil, false
	}

	if freeze == nil || !freeze.isFrozen() {
		return nil, true
	}

	if m.trigger == "" {
		return nil, false
	}

	if freeze.isOwner(m.trigger) {
		return nil, true
	}

	return []string{
		fmt.Sprintf(msgFrozenWithOwner, strings.Join(freeze.Owner, ", ")),
	}, false
}

func (m *mergeHelper) getFreezeProjectID(org, repo string) (int, error) {
	grps, err := m.cli.GetGroups()
	if err != nil || len(grps) == 0 {
		return 0, err
	}

	gid := 0

	for _, g := range grps {
		if g.Name == org {
			gid = g.ID
		}
	}

	prjs, err := m.cli.GetProjects(gid)
	if err != nil || len(prjs) == 0 {
		return 0, err
	}

	pid := 0
	for _, p := range prjs {
		if p.Name == repo {
			pid = p.ID
		}
	}

	return pid, nil
}

func (m *mergeHelper) getFreezeInfo(log *logrus.Entry) (*freezeItem, error) {
	branch := m.mr.TargetBranch
	for _, v := range m.cfg.FreezeFile {
		fc, err := m.getFreezeContent(v)
		if err != nil {
			log.Errorf("get freeze file:%s, err:%s", v.toString(), err.Error())
			return nil, err
		}

		if v := fc.getFreezeItem(m.org, branch); v != nil {
			return v, nil
		}
	}

	return nil, nil
}

func (m *mergeHelper) getFreezeContent(f freezeFile) (freezeContent, error) {
	var fc freezeContent

	pid, err := m.getFreezeProjectID(f.Owner, f.Repo)
	if err != nil || pid == 0 {
		return fc, err
	}

	c, err := m.cli.GetPathContent(pid, f.Path, f.Branch)
	if err != nil {
		return fc, err
	}

	b, err := base64.StdEncoding.DecodeString(c.Content)
	if err != nil {
		return fc, err
	}

	err = yaml.Unmarshal(b, &fc)

	return fc, err
}

func (m *mergeHelper) getMRLabels() sets.String {
	if m.trigger == "" {
		return sets.NewString(m.mr.Labels...)
	}

	mrLabels, err := m.cli.GetMergeRequestLabels(m.pid, m.mrID)
	if err != nil {
		return sets.NewString(m.mr.Labels...)
	}

	labels := sets.NewString(mrLabels...)

	return labels
}

func (m *mergeHelper) genMergeDesc() string {
	comments, err := m.cli.ListMergeRequestComments(m.pid, m.mrID)
	if err != nil || len(comments) == 0 {
		return ""
	}

	f := func(comment gitlab.Note, reg *regexp.Regexp) bool {
		return reg.MatchString(comment.Body) &&
			comment.UpdatedAt.String() == comment.CreatedAt.String() &&
			comment.Author.Username != m.author
	}

	reviewers := sets.NewString()
	signers := sets.NewString()

	for _, c := range comments {
		if f(*c, regAddLgtm) {
			reviewers.Insert(c.Author.Username)
		}

		if f(*c, regAddApprove) {
			signers.Insert(c.Author.Username)
		}
	}

	if len(signers) == 0 && len(reviewers) == 0 {
		return ""
	}

	if m.org == "openeuler" && m.repo == "kernel" {
		f, err := m.cli.GetPathContent(m.pid, "sig/Kernel/sig-info.yaml", "master")
		if err != nil {
			return ""
		}

		ct, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return ""
		}

		var s SigInfos

		if err := yaml.Unmarshal(ct, &s); err != nil {
			return ""
		}

		nameEmail := make(map[string]string, len(s.Maintainers))
		for _, ms := range s.Maintainers {
			nameEmail[ms.GiteeID] = fmt.Sprintf("%s <%s>", ms.Name, ms.Email)
		}

		for _, i := range s.Repositories {
			for _, j := range i.Committers {
				nameEmail[j.GiteeID] = fmt.Sprintf("%s <%s>", j.Name, j.Email)
			}
		}

		reviewersInfo := sets.NewString()
		for r, _ := range reviewers {
			if v, ok := nameEmail[r]; ok {
				reviewersInfo.Insert(v)
			}
		}

		signersInfo := sets.NewString()
		for s, _ := range signers {
			if v, ok := nameEmail[s]; ok {
				signersInfo.Insert(v)
			}
		}

		reviewedUserInfo := make([]string, 0)
		for _, item := range reviewersInfo.UnsortedList() {
			reviewedUserInfo = append(reviewedUserInfo, fmt.Sprintf("Reviewed-by: %s \n", item))
		}

		signedOffUserInfo := make([]string, 0)
		for _, item := range signersInfo.UnsortedList() {
			signedOffUserInfo = append(signedOffUserInfo, fmt.Sprintf("Signed-off-by: %s \n", item))
		}

		return fmt.Sprintf(
			"\nFrom: @%s \n%s%s",
			m.author,
			strings.Join(reviewedUserInfo, ""),
			strings.Join(signedOffUserInfo, ""),
		)

	}

	return fmt.Sprintf(
		"From: @%s \nReviewed-by: @%s \nSigned-off-by: @%s \n",
		m.author,
		strings.Join(reviewers.UnsortedList(), ", @"),
		strings.Join(signers.UnsortedList(), ", @"),
	)
}

func isLabelMatched(labels sets.String, cfg *botConfig, ops []*gitlab.LabelEvent, log *logrus.Entry) []string {
	var reasons []string

	needs := sets.NewString(approvedLabel)
	needs.Insert(cfg.LabelsForMerge...)

	if ln := cfg.LgtmCountsRequired; ln == 1 {
		needs.Insert(lgtmLabel)
	} else {
		v := getLGTMLabelsOnPR(labels)
		if n := uint(len(v)); n < ln {
			reasons = append(reasons, fmt.Sprintf(msgNotEnoughLGTMLabel, ln, n)+"\n")
		}
	}

	s := checkLabelsLegal(labels, needs, ops, log)
	if s != "" {
		reasons = append(reasons, s+"\n")
	}

	if v := needs.Difference(labels); v.Len() > 0 {
		reasons = append(reasons, fmt.Sprintf(
			msgMissingLabels, strings.Join(v.UnsortedList(), ", "),
		))
	}

	if len(cfg.MissingLabelsForMerge) > 0 {
		missing := sets.NewString(cfg.MissingLabelsForMerge...)
		if v := missing.Intersection(labels); v.Len() > 0 {
			reasons = append(reasons, fmt.Sprintf(
				msgInvalidLabels, strings.Join(v.UnsortedList(), ", "),
			))
		}
	}

	return reasons
}

type labelLog struct {
	label string
	who   string
	t     time.Time
}

func getLatestLog(ops []*gitlab.LabelEvent, label string, log *logrus.Entry) (labelLog, bool) {
	var t time.Time

	index := -1

	for i := range ops {
		op := ops[i]

		if op.Action != ActionAddLabel || !strings.Contains(op.Label.Name, label) {
			continue
		}

		ut, err := time.Parse(time.RFC3339, op.CreatedAt.Format(time.RFC3339))
		if err != nil {
			log.Warnf("parse time:%s failed", op.CreatedAt)

			continue
		}

		if index < 0 || ut.After(t) {
			t = ut
			index = i
		}
	}

	if index >= 0 {
		if user := ops[index].User; user.Username != "" {
			return labelLog{
				label: label,
				t:     t,
				who:   user.Username,
			}, true
		}
	}

	return labelLog{}, false
}

func checkLabelsLegal(labels sets.String, needs sets.String, ops []*gitlab.LabelEvent, log *logrus.Entry) string {
	f := func(label string) string {
		v, b := getLatestLog(ops, label, log)
		if !b {
			return fmt.Sprintf("The corresponding operation log is missing. you should delete " +
				"the label and add it again by correct way")
		}

		if v.who != legalLabelsAddedBy {
			if strings.HasPrefix(v.label, "openeuler-cla/") {
				return fmt.Sprintf("%s You can't add %s by yourself, "+
					"please remove it and use /check-cla to add it", v.who, v.label)
			}

			return fmt.Sprintf("%s You can't add %s by yourself, please contact the maintainers", v.who, v.label)
		}

		return ""
	}

	v := make([]string, 0, len(labels))

	for label := range labels {
		if ok := needs.Has(label); ok || strings.HasPrefix(label, lgtmLabel) {
			if s := f(label); s != "" {
				v = append(v, fmt.Sprintf("%s: %s", label, s))
			}
		}
	}

	if n := len(v); n > 0 {
		s := "label is"

		if n > 1 {
			s = "labels are"
		}

		return fmt.Sprintf("**The following %s not ready**.\n\n%s", s, strings.Join(v, "\n\n"))
	}

	return ""
}
