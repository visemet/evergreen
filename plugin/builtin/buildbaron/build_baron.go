package buildbaron

import (
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/event"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/plugin"
	"github.com/evergreen-ci/evergreen/thirdparty"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/gorilla/mux"
	"github.com/mitchellh/mapstructure"
	"github.com/mongodb/grip"
)

func init() {
	plugin.Publish(&BuildBaronPlugin{})
}

const (
	PluginName  = "buildbaron"
	JIRAFailure = "Error searching jira for ticket"
	JQLBFQuery  = "(project in (%v)) and ( %v ) order by updatedDate desc"

	msPerNS     = 1000 * 1000
	maxNoteSize = 16 * 1024 // 16KB
)

type bbPluginOptions struct {
	Host     string
	Username string
	Password string
	Projects map[string]bbProject
}

type bbProject struct {
	TicketCreateProject  string   `mapstructure:"ticket_create_project"`
	TicketSearchProjects []string `mapstructure:"ticket_search_projects"`
}

type BuildBaronPlugin struct {
	opts        *bbPluginOptions
	jiraHandler thirdparty.JiraHandler
}

// A regex that matches either / or \ for splitting directory paths
// on either windows or linux paths.
var eitherSlash *regexp.Regexp = regexp.MustCompile(`[/\\]`)

func (bbp *BuildBaronPlugin) Name() string {
	return PluginName
}

// GetUIHandler adds a path for looking up build failures in JIRA.
func (bbp *BuildBaronPlugin) GetUIHandler() http.Handler {
	if bbp.opts == nil {
		panic("build baron plugin missing configuration")
	}
	r := mux.NewRouter()
	r.Path("/jira_bf_search/{task_id}/{execution}").HandlerFunc(bbp.buildFailuresSearch)
	r.Path("/created_tickets/{task_id}").HandlerFunc(bbp.getCreatedTickets)
	r.Path("/note/{task_id}").Methods("GET").HandlerFunc(bbp.getNote)
	r.Path("/note/{task_id}").Methods("PUT").HandlerFunc(bbp.saveNote)
	r.Path("/file_ticket").Methods("POST").HandlerFunc(bbp.fileTicket)
	return r
}

func (bbp *BuildBaronPlugin) Configure(conf map[string]interface{}) error {
	// pull out options needed from config file (JIRA authentication info, and list of projects)
	bbpOptions := &bbPluginOptions{}

	err := mapstructure.Decode(conf, bbpOptions)
	if err != nil {
		return err
	}
	if bbpOptions.Host == "" || bbpOptions.Username == "" || bbpOptions.Password == "" {
		return fmt.Errorf("Host, username, and password in config must not be blank")
	}
	if len(bbpOptions.Projects) == 0 {
		return fmt.Errorf("Must specify at least 1 Evergreen project")
	}
	for _, proj := range bbpOptions.Projects {
		if proj.TicketCreateProject == "" {
			return fmt.Errorf("ticket_create_project cannot be blank")
		}
		if len(proj.TicketSearchProjects) == 0 {
			return fmt.Errorf("ticket_search_projects cannot be empty")
		}
	}
	bbp.opts = bbpOptions
	bbp.jiraHandler = thirdparty.NewJiraHandler(
		bbp.opts.Host,
		bbp.opts.Username,
		bbp.opts.Password,
	)
	return nil
}

func (bbp *BuildBaronPlugin) GetPanelConfig() (*plugin.PanelConfig, error) {
	return &plugin.PanelConfig{
		Panels: []plugin.UIPanel{
			{
				Page:      plugin.TaskPage,
				Position:  plugin.PageRight,
				PanelHTML: template.HTML(`<div ng-include="'/plugin/buildbaron/static/partials/task_build_baron.html'"></div>`),
				Includes: []template.HTML{
					template.HTML(`<link href="/plugin/buildbaron/static/css/task_build_baron.css" rel="stylesheet"/>`),
					template.HTML(`<script type="text/javascript" src="/plugin/buildbaron/static/js/task_build_baron.js"></script>`),
				},
				DataFunc: func(context plugin.UIContext) (interface{}, error) {
					_, enabled := bbp.opts.Projects[context.ProjectRef.Identifier]
					return struct {
						Enabled bool `json:"enabled"`
					}{enabled}, nil
				},
			},
		},
	}, nil
}

type searchReturnInfo struct {
	Issues []thirdparty.JiraTicket `json:"issues"`
	Search string                  `json:"search"`
}

// BuildFailuresSearchHandler handles the requests of searching jira in the build
//  failures project
func (bbp *BuildBaronPlugin) buildFailuresSearch(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]
	exec := mux.Vars(r)["execution"]
	oldId := fmt.Sprintf("%v_%v", taskId, exec)
	t, err := task.FindOneOld(task.ById(oldId))
	if err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// if the archived task was not found, we must be looking for the most recent exec
	if t == nil {
		t, err = task.FindOne(task.ById(taskId))
		if err != nil {
			util.WriteJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	bbProj, ok := bbp.opts.Projects[t.Project]
	if !ok {
		util.WriteJSON(w, http.StatusInternalServerError,
			fmt.Sprintf("Corresponding JIRA project for %v not found", t.Project))
		return
	}
	jql := taskToJQL(t, bbProj.TicketSearchProjects)

	results, err := bbp.jiraHandler.JQLSearch(jql, 0, -1)
	if err != nil {
		message := fmt.Sprintf("%v: %v, %v", JIRAFailure, err, jql)
		grip.Error(message)
		util.WriteJSON(w, http.StatusInternalServerError, message)
		return
	}
	util.WriteJSON(w, http.StatusOK, searchReturnInfo{Issues: results.Issues, Search: jql})
}

func (bbp *BuildBaronPlugin) getCreatedTickets(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]

	events, err := event.Find(event.AllLogCollection, event.TaskEventsForId(taskId))
	if err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	var results []thirdparty.JiraTicket
	var searchTickets []string
	for _, evt := range events {
		data := evt.Data.(*event.TaskEventData)
		if evt.EventType == event.TaskJiraAlertCreated {
			searchTickets = append(searchTickets, data.JiraIssue)
		}
	}

	for _, ticket := range searchTickets {
		jiraIssue, err := bbp.jiraHandler.GetJIRATicket(ticket)
		if err != nil {
			util.WriteJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if jiraIssue == nil {
			continue
		}
		results = append(results, *jiraIssue)
	}

	util.WriteJSON(w, http.StatusOK, results)
}

// getNote retrieves the latest note from the database.
func (bbp *BuildBaronPlugin) getNote(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]
	n, err := model.NoteForTask(taskId)
	if err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == nil {
		util.WriteJSON(w, http.StatusOK, "")
		return
	}
	util.WriteJSON(w, http.StatusOK, n)
}

// saveNote reads a request containing a note's content along with the last seen
// edit time and updates the note in the database.
func (bbp *BuildBaronPlugin) saveNote(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]
	n := &model.Note{}
	if err := util.ReadJSONInto(r.Body, n); err != nil {
		util.WriteJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// prevent incredibly large notes
	if len(n.Content) > maxNoteSize {
		util.WriteJSON(w, http.StatusBadRequest, "note is too large")
		return
	}

	// We need to make sure the user isn't blowing away a new edit,
	// so we load the existing note. If the user's last seen edit time is less
	// than the most recent edit, we error with a helpful message.
	old, err := model.NoteForTask(taskId)
	if err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// we compare times by millisecond rather than nanosecond so we can
	// work around the rounding that occurs when javascript forces these
	// large values into in float type.
	if old != nil && n.UnixNanoTime/msPerNS != old.UnixNanoTime/msPerNS {
		util.WriteJSON(w, http.StatusBadRequest,
			"this note has already been edited. Please refresh and try again.")
		return
	}

	n.TaskId = taskId
	n.UnixNanoTime = time.Now().UnixNano()
	if err := n.Upsert(); err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	util.WriteJSON(w, http.StatusOK, n)
}

// Generates a jira JQL string from the task
// When we search in jira for a task we search in the specified JIRA project
// If there are any test results, then we only search by test file
// name of all of the failed tests.
// Otherwise we search by the task name.
func taskToJQL(t *task.Task, searchProjects []string) string {
	var jqlParts []string
	var jqlClause string
	for _, testResult := range t.LocalTestResults {
		if testResult.Status == evergreen.TestFailedStatus {
			fileParts := eitherSlash.Split(testResult.TestFile, -1)
			jqlParts = append(jqlParts, fmt.Sprintf("text~\"%v\"", fileParts[len(fileParts)-1]))
		}
	}
	if jqlParts != nil {
		jqlClause = strings.Join(jqlParts, " or ")
	} else {
		jqlClause = fmt.Sprintf("text~\"%v\"", t.DisplayName)
	}

	return fmt.Sprintf(JQLBFQuery, strings.Join(searchProjects, ", "), jqlClause)
}
