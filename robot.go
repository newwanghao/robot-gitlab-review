package main

import (
	"github.com/opensourceways/community-robot-lib/gitlabclient"
	"github.com/xanzy/go-gitlab"

	"github.com/opensourceways/community-robot-lib/utils"
	cache "github.com/opensourceways/repo-file-cache/sdk"
	"github.com/sirupsen/logrus"
)

const botName = "review"

type iClient interface {
	GetMergeRequestLabels(projectID interface{}, mrID int) (gitlab.Labels, error)
	CreateMergeRequestComment(projectID interface{}, mrID int, comment string) error
	RemoveMergeRequestLabel(projectID interface{}, mrID int, labels gitlab.Labels) error
	GetProjectLabels(projectID interface{}) ([]*gitlab.Label, error)
	CreateProjectLabel(pid interface{}, label, color string) error
	AddMergeRequestLabel(projectID interface{}, mrID int, labels gitlab.Labels) error
	GetUserPermissionOfProject(projectID interface{}, userID int) (bool, error)
	GetMergeRequestChanges(projectID interface{}, mrID int) ([]string, error)
	MergeMergeRequest(projectID interface{}, mrID int) error
	ListMergeRequestComments(projectID interface{}, mrID int) ([]*gitlab.Note, error)
	GetMergeRequestLabelChanges(projectID interface{}, mrID int) ([]*gitlab.LabelEvent, error)
	GetMergeRequest(projectID interface{}, mrID int) (gitlab.MergeRequest, error)
	UpdateMergeRequest(projectID interface{}, mrID int, options gitlab.UpdateMergeRequestOptions) (gitlab.MergeRequest, error)
	GetPathContent(projectID interface{}, file, branch string) (*gitlab.File, error)
	GetDirectoryTree(projectID interface{}, opts gitlab.ListTreeOptions) ([]*gitlab.TreeNode, error)
	GetGroups() ([]*gitlab.Group, error)
	GetProjects(gid interface{}) ([]*gitlab.Project, error)
}

func newRobot(cli iClient, cacheCli *cache.SDK, gc func() (*configuration, error)) *robot {
	return &robot{cli: cli, cacheCli: cacheCli, getConfig: gc}
}

type robot struct {
	cli       iClient
	cacheCli  *cache.SDK
	getConfig func() (*configuration, error)
}

func (bot *robot) HandleMergeEvent(e *gitlab.MergeEvent, log *logrus.Entry) error {
	org, repo := gitlabclient.GetMROrgAndRepo(e)
	c, err := bot.getConfig()
	if err != nil {
		return err
	}
	botCfg := c.configFor(org, repo)

	merr := utils.NewMultiErrors()
	if err := bot.clearLabel(e); err != nil {
		merr.AddError(err)
	}

	if err := bot.doRetest(e); err != nil {
		merr.AddError(err)
	}

	if err := bot.checkReviewer(e, botCfg); err != nil {
		merr.AddError(err)
	}

	if err := bot.handleLabelUpdate(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	return merr.Err()
}

func (bot *robot) HandleMergeCommentEvent(e *gitlab.MergeCommentEvent, log *logrus.Entry) error {
	org, repo := gitlabclient.GetMRCommentOrgAndRepo(e)
	c, err := bot.getConfig()
	if err != nil {
		return err
	}
	botCfg := c.configFor(org, repo)

	merr := utils.NewMultiErrors()
	if err := bot.handleLGTM(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.handleApprove(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.handleCheckPR(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.removeInvalidCLA(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.handleRebase(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.handleFlattened(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.removeRebase(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.removeFlattened(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	if err = bot.handleACK(e, botCfg, log); err != nil {
		merr.AddError(err)
	}

	return merr.Err()
}
