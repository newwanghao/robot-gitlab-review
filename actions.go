package main

import (
	"encoding/base64"
	"fmt"
	"github.com/opensourceways/community-robot-lib/gitlabclient"
	"github.com/sirupsen/logrus"
	"github.com/xanzy/go-gitlab"
	"k8s.io/apimachinery/pkg/util/sets"
	"regexp"
	"sigs.k8s.io/yaml"
	"strings"
)

const (
	retestCommand     = "/retest"
	removeClaCommand  = "/cla cancel"
	rebaseCommand     = "/rebase"
	removeRebase      = "/rebase cancel"
	removeSquash      = "/squash cancel"
	baseMergeMethod   = "merge"
	squashCommand     = "/squash"
	removeLabel       = "openeuler-cla/yes"
	ackLabel          = "Acked"
	msgNotSetReviewer = "**@%s** Thank you for submitting a PullRequest. It is detected that you have not set a reviewer, please set a one."
)

var (
	regAck     = regexp.MustCompile(`(?mi)^/ack\s*$`)
	ackCommand = regexp.MustCompile(`(?mi)^/ack\s*$`)
)

func (bot *robot) doRetest(e *gitlab.MergeEvent) error {
	if e.ObjectAttributes.State != "opened" || !gitlabclient.CheckSourceBranchChanged(e) {
		return nil
	}

	pid := e.Project.ID
	mrID := e.ObjectAttributes.IID

	return bot.cli.CreateMergeRequestComment(pid, mrID, retestCommand)
}

func (bot *robot) checkReviewer(e *gitlab.MergeEvent, cfg *botConfig) error {
	if cfg.UnableCheckingReviewerForPR || e.ObjectAttributes.State != "opened" {
		return nil
	}

	if e != nil && len(e.ObjectAttributes.AssigneeIDs) > 0 {
		return nil
	}

	pid := e.Project.ID
	mrID := e.ObjectAttributes.IID
	author := gitlabclient.GetMRAuthor(e)

	return bot.cli.CreateMergeRequestComment(
		pid, mrID,
		fmt.Sprintf(msgNotSetReviewer, author),
	)
}

func (bot *robot) clearLabel(e *gitlab.MergeEvent) error {
	if e.ObjectAttributes.State != "opened" || !gitlabclient.CheckSourceBranchChanged(e) {
		return nil
	}

	pid := e.Project.ID
	mrID := e.ObjectAttributes.IID
	labelSet := sets.NewString()
	mrLabels, err := bot.cli.GetMergeRequestLabels(pid, mrID)
	if err != nil {
		return err
	}
	labelSet.Insert(mrLabels...)
	v := getLGTMLabelsOnPR(labelSet)

	if labelSet.Has(approvedLabel) {
		v = append(v, approvedLabel)
	}

	if len(v) > 0 {

		if err := bot.cli.RemoveMergeRequestLabel(pid, mrID, v); err != nil {
			return err
		}

		return bot.cli.CreateMergeRequestComment(
			pid, mrID,
			fmt.Sprintf(commentClearLabel, strings.Join(v, ", ")),
		)
	}

	return nil
}

func (bot *robot) removeInvalidCLA(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != "opened" ||
		e.ObjectKind != "note" ||
		gitlabclient.GetMRCommentBody(e) != removeClaCommand {
		return nil
	}

	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenterID := gitlabclient.GetMRCommentAuthorID(e)
	commenter := gitlabclient.GetMRCommentAuthor(e)

	hasPermission, err := bot.hasPermission(org, repo, commenter, commenterID, false, e, cfg, log)
	if err != nil {
		return err
	}

	if !hasPermission {
		return nil
	}

	return bot.cli.RemoveMergeRequestLabel(pid, number, []string{removeLabel})
}

func (bot *robot) handleRebase(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != "opened" ||
		e.ObjectKind != "note" ||
		gitlabclient.GetMRCommentBody(e) != rebaseCommand {
		return nil
	}

	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenterID := gitlabclient.GetMRCommentAuthorID(e)
	commenter := gitlabclient.GetMRCommentAuthor(e)

	hasPermission, err := bot.hasPermission(org, repo, commenter, commenterID, false, e, cfg, log)
	if err != nil {
		return err
	}

	if !hasPermission {
		return nil
	}

	var prLabels map[string]string
	labels, err := bot.cli.GetMergeRequestLabels(pid, number)
	if err == nil {
		for _, l := range labels {
			prLabels[l] = l
		}
	}
	if _, ok := prLabels["merge/squash"]; ok {
		return bot.cli.CreateMergeRequestComment(pid, number,
			"Please use **/squash cancel** to remove **merge/squash** label, and try **/rebase** again")
	}

	return bot.cli.AddMergeRequestLabel(pid, number, []string{"merge/rebase"})
}

func (bot *robot) handleFlattened(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != "opened" ||
		e.ObjectKind != "note" ||
		gitlabclient.GetMRCommentBody(e) != rebaseCommand {
		return nil
	}

	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenterID := gitlabclient.GetMRCommentAuthorID(e)
	commenter := gitlabclient.GetMRCommentAuthor(e)

	hasPermission, err := bot.hasPermission(org, repo, commenter, commenterID, false, e, cfg, log)
	if err != nil {
		return err
	}

	if !hasPermission {
		return nil
	}

	var prLabels map[string]string
	labels, err := bot.cli.GetMergeRequestLabels(pid, number)
	if err == nil {
		for _, l := range labels {
			prLabels[l] = l
		}
	}
	if _, ok := prLabels["merge/rebase"]; ok {
		return bot.cli.CreateMergeRequestComment(pid, number,
			"Please use **/rebase cancel** to remove **merge/rebase** label, and try **/squash** again")
	}

	return bot.cli.AddMergeRequestLabel(pid, number, []string{"merge/squash"})
}

func (bot *robot) genMergeMethod(e *gitlab.MergeRequest, org, repo string, log *logrus.Entry) string {
	mergeMethod := "merge"

	number := e.IID
	pid := e.ProjectID

	var prLabels []string
	labels, err := bot.cli.GetMergeRequestLabels(pid, number)
	if err == nil {
		for _, l := range labels {
			prLabels = append(prLabels, l)
		}
	}
	sigLabel := ""

	for _, p := range prLabels {
		if strings.HasPrefix(p, "merge/") {
			if strings.Split(p, "/")[1] == "squash" {
				return "squash"
			}

			return strings.Split(p, "/")[1]
		}

		if strings.HasPrefix(p, "sig/") {
			sigLabel = p
		}
	}

	if sigLabel == "" {
		return mergeMethod
	}

	sig := strings.Split(sigLabel, "/")[1]
	filePath := fmt.Sprintf("sig/%s/%s/%s/%s", sig, org, strings.ToLower(repo[0:1]), fmt.Sprintf("%s.yaml", repo))

	c, err := bot.cli.GetPathContent(pid, filePath, "master")
	if err != nil {
		log.Infof("get repo %s failed, because of %v", fmt.Sprintf("%s-%s", org, repo), err)

		return mergeMethod
	}

	mergeMethod = bot.decodeRepoYaml(c, log)

	return mergeMethod
}

func (bot *robot) removeRebase(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != "opened" ||
		e.ObjectKind != "note" ||
		gitlabclient.GetMRCommentBody(e) != removeRebase {
		return nil
	}

	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenterID := gitlabclient.GetMRCommentAuthorID(e)
	commenter := gitlabclient.GetMRCommentAuthor(e)

	hasPermission, err := bot.hasPermission(org, repo, commenter, commenterID, false, e, cfg, log)
	if err != nil {
		return err
	}

	if !hasPermission {
		return nil
	}

	return bot.cli.RemoveMergeRequestLabel(pid, number, []string{"merge/rebase"})
}

func (bot *robot) removeFlattened(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != "opened" ||
		e.ObjectKind != "note" ||
		gitlabclient.GetMRCommentBody(e) != removeSquash {
		return nil
	}

	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenterID := gitlabclient.GetMRCommentAuthorID(e)
	commenter := gitlabclient.GetMRCommentAuthor(e)

	hasPermission, err := bot.hasPermission(org, repo, commenter, commenterID, false, e, cfg, log)
	if err != nil {
		return err
	}

	if !hasPermission {
		return nil
	}

	return bot.cli.RemoveMergeRequestLabel(pid, number, []string{"merge/squash"})
}

func (bot *robot) handleACK(e *gitlab.MergeCommentEvent, cfg *botConfig, log *logrus.Entry) error {
	if e.MergeRequest.State != "opened" ||
		e.ObjectKind != "note" {
		return nil
	}

	if !ackCommand.MatchString(gitlabclient.GetMRCommentBody(e)) {
		return nil
	}

	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	number := e.MergeRequest.IID
	pid := e.ProjectID
	commenterID := gitlabclient.GetMRCommentAuthorID(e)
	commenter := gitlabclient.GetMRCommentAuthor(e)

	hasPermission, err := bot.hasPermission(org, repo, commenter, commenterID, false, e, cfg, log)
	if err != nil {
		return err
	}

	if !hasPermission {
		return nil
	}

	return bot.cli.AddMergeRequestLabel(pid, number, []string{ackLabel})
}

func (bot *robot) decodeRepoYaml(content *gitlab.File, log *logrus.Entry) string {
	c, err := base64.StdEncoding.DecodeString(content.Content)
	if err != nil {
		log.WithError(err).Error("decode file")

		return baseMergeMethod
	}

	var r Repository
	if err = yaml.Unmarshal(c, &r); err != nil {
		log.WithError(err).Error("code yaml file")

		return baseMergeMethod
	}

	if r.MergeMethod != "" {
		if r.MergeMethod == "rebase" || r.MergeMethod == "squash" {
			return r.MergeMethod
		}
	}

	return baseMergeMethod
}
