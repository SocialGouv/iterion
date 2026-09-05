package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// GitHub Projects v2 — the implementation of forge.BoardClient (ADR-097).
//
// Two properties of this API shape the code below:
//
//   - Everything is addressed by opaque node ids (`PVTSSF_lADO…`). None of
//     them is guessable or stable across projects, so every id here is
//     DISCOVERED by name through GetProject and cached by the caller; nothing
//     is hardcoded.
//   - A field VALUE carries its own `updatedAt`, distinct from the item's.
//     That per-value timestamp is what the two-way status sync compares, so it
//     is decoded and carried rather than collapsed onto the item.

// projectFieldsQuery reads a board's identity + field schema. The
// `... on ProjectV2SingleSelectField` fragment is what exposes the option ids;
// ProjectV2FieldCommon alone would give name/id and hide the options.
const projectFieldsQuery = `query($login:String!,$number:Int!,$fields:Int!){
  %s(login:$login){
    projectV2(number:$number){
      id title number url
      fields(first:$fields){
        nodes{
          __typename
          ... on ProjectV2FieldCommon { id name dataType }
          ... on ProjectV2SingleSelectField { id name dataType options { id name } }
        }
      }
    }
  }
}`

// projectItemsQuery reads one page of items with their field values. Only the
// two value kinds iterion consumes are selected (single-select for status and
// the label fields, text for a title fallback); every other value kind decodes
// to an entry with no field, which normalization drops.
const projectItemsQuery = `query($login:String!,$number:Int!,$first:Int!,$after:String,$values:Int!){
  %s(login:$login){
    projectV2(number:$number){
      items(first:$first, after:$after){
        pageInfo{ hasNextPage endCursor }
        nodes{
          id updatedAt isArchived type
          content{
            __typename
            ... on Issue { number title url state repository { nameWithOwner } }
            ... on PullRequest { number title url state repository { nameWithOwner } }
            ... on DraftIssue { title }
          }
          fieldValues(first:$values){
            nodes{
              __typename
              ... on ProjectV2ItemFieldSingleSelectValue { name optionId updatedAt field { ... on ProjectV2FieldCommon { id name } } }
              ... on ProjectV2ItemFieldTextValue { text updatedAt field { ... on ProjectV2FieldCommon { id name } } }
            }
          }
        }
      }
    }
  }
}`

const issueNodeIDQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){ issue(number:$number){ id } }
}`

const addProjectItemMutation = `mutation($projectId:ID!,$contentId:ID!){
  addProjectV2ItemById(input:{projectId:$projectId, contentId:$contentId}){
    item{
      id updatedAt isArchived type
      content{
        __typename
        ... on Issue { number title url state repository { nameWithOwner } }
        ... on PullRequest { number title url state repository { nameWithOwner } }
        ... on DraftIssue { title }
      }
      fieldValues(first:20){
        nodes{
          __typename
          ... on ProjectV2ItemFieldSingleSelectValue { name optionId updatedAt field { ... on ProjectV2FieldCommon { id name } } }
        }
      }
    }
  }
}`

const setSingleSelectMutation = `mutation($projectId:ID!,$itemId:ID!,$fieldId:ID!,$optionId:String!){
  updateProjectV2ItemFieldValue(input:{
    projectId:$projectId, itemId:$itemId, fieldId:$fieldId,
    value:{ singleSelectOptionId:$optionId }
  }){ projectV2Item { id } }
}`

// Paging defaults. A project's field schema is small (GitHub's own limit is 50
// fields); items page at 100, GitHub's maximum.
const (
	projectFieldsPageSize    = 50
	projectItemsPageSize     = 100
	projectItemValuesPerItem = 20
)

// ownerEntry picks the GraphQL root field for the owner kind. GitHub exposes
// projectV2 on Organization and User separately; there is no shared interface
// to query through, so the entry point is substituted into the query.
func ownerEntry(kind forge.ProjectOwnerKind) (string, error) {
	switch kind.OrDefault() {
	case forge.ProjectOwnerOrg:
		return "organization", nil
	case forge.ProjectOwnerUser:
		return "user", nil
	default:
		return "", fmt.Errorf("github: unknown project owner kind %q", kind)
	}
}

// wire shapes -----------------------------------------------------------------

type wireProjectFields struct {
	Organization *wireProjectHolder `json:"organization"`
	User         *wireProjectHolder `json:"user"`
}

func (w wireProjectFields) project() *wireProject {
	if w.Organization != nil {
		return w.Organization.ProjectV2
	}
	if w.User != nil {
		return w.User.ProjectV2
	}
	return nil
}

type wireProjectHolder struct {
	ProjectV2 *wireProject `json:"projectV2"`
}

type wireProject struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Fields struct {
		Nodes []wireProjectField `json:"nodes"`
	} `json:"fields"`
	Items wireItemConnection `json:"items"`
}

type wireProjectField struct {
	TypeName string `json:"__typename"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	DataType string `json:"dataType"`
	Options  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"options"`
}

type wireItemConnection struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []wireItem `json:"nodes"`
}

type wireItem struct {
	ID         string    `json:"id"`
	UpdatedAt  time.Time `json:"updatedAt"`
	IsArchived bool      `json:"isArchived"`
	Type       string    `json:"type"`
	Content    struct {
		TypeName   string `json:"__typename"`
		Number     int    `json:"number"`
		Title      string `json:"title"`
		URL        string `json:"url"`
		State      string `json:"state"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"content"`
	FieldValues struct {
		Nodes []wireItemFieldValue `json:"nodes"`
	} `json:"fieldValues"`
}

type wireItemFieldValue struct {
	TypeName  string    `json:"__typename"`
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	OptionID  string    `json:"optionId"`
	UpdatedAt time.Time `json:"updatedAt"`
	Field     struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"field"`
}

// normalization ---------------------------------------------------------------

func normalizeProject(w *wireProject) forge.Project {
	p := forge.Project{ID: w.ID, Number: w.Number, Title: w.Title, URL: w.URL}
	for _, f := range w.Fields.Nodes {
		if f.ID == "" {
			continue // a field kind the query selected nothing for
		}
		pf := forge.ProjectField{ID: f.ID, Name: f.Name, DataType: f.DataType}
		for _, o := range f.Options {
			pf.Options = append(pf.Options, forge.ProjectFieldOption{ID: o.ID, Name: o.Name})
		}
		p.Fields = append(p.Fields, pf)
	}
	return p
}

// normalizeContentKind maps GitHub's content __typename onto iterion's own
// vocabulary. An unknown typename yields "" — reported, never guessed.
func normalizeContentKind(typename string) string {
	switch typename {
	case "Issue":
		return forge.ProjectContentIssue
	case "PullRequest":
		return forge.ProjectContentPull
	case "DraftIssue":
		return forge.ProjectContentDraft
	default:
		return ""
	}
}

func normalizeItem(w wireItem) forge.ProjectItem {
	it := forge.ProjectItem{
		ID:        w.ID,
		Archived:  w.IsArchived,
		UpdatedAt: w.UpdatedAt,
		Content: forge.ProjectItemContent{
			Kind:   normalizeContentKind(w.Content.TypeName),
			Repo:   w.Content.Repository.NameWithOwner,
			Number: w.Content.Number,
			Title:  w.Content.Title,
			URL:    w.Content.URL,
			State:  strings.ToLower(w.Content.State),
		},
	}
	for _, v := range w.FieldValues.Nodes {
		if v.Field.ID == "" {
			// A value kind the query did not select a field for (repository,
			// milestone, …). It carries no field identity, so it cannot be
			// addressed; dropping it is not a loss.
			continue
		}
		fv := forge.ProjectItemField{
			FieldID:   v.Field.ID,
			FieldName: v.Field.Name,
			OptionID:  v.OptionID,
			UpdatedAt: v.UpdatedAt,
		}
		switch {
		case v.Name != "":
			fv.Value = v.Name
		default:
			fv.Value = v.Text
		}
		it.Fields = append(it.Fields, fv)
	}
	return it
}

// projectFromErr maps a GraphQL NOT_FOUND on a project lookup onto the forge
// sentinel, keeping GitHub's own message as the cause.
func projectFromErr(err error, ref forge.ProjectRef) error {
	var g *GraphQLErrors
	if errors.As(err, &g) && g.HasType("NOT_FOUND") {
		return fmt.Errorf("%w: %s (%s): %w", forge.ErrProjectNotFound, ref.String(), ref.OwnerKind.OrDefault(), err)
	}
	return err
}

// BoardClient implementation ---------------------------------------------------

// GetProject resolves a board by owner+number and returns its field schema,
// including every single-select's option set — the discovery step that keeps
// field and option ids out of the codebase.
func (c *AdminClient) GetProject(ctx context.Context, ref forge.ProjectRef) (forge.Project, error) {
	if err := ref.Validate(); err != nil {
		return forge.Project{}, err
	}
	entry, err := ownerEntry(ref.OwnerKind)
	if err != nil {
		return forge.Project{}, err
	}
	var out wireProjectFields
	q := fmt.Sprintf(projectFieldsQuery, entry)
	vars := map[string]any{"login": ref.Owner, "number": ref.Number, "fields": projectFieldsPageSize}
	if err := c.graphQLOp(ctx, "get project", q, vars, &out); err != nil {
		return forge.Project{}, projectFromErr(err, ref)
	}
	p := out.project()
	if p == nil || p.ID == "" {
		return forge.Project{}, fmt.Errorf("%w: %s (%s)", forge.ErrProjectNotFound, ref.String(), ref.OwnerKind.OrDefault())
	}
	return normalizeProject(p), nil
}

// ListProjectItems returns one page of the board's items with their field
// values. Archived items are flagged, not filtered.
func (c *AdminClient) ListProjectItems(ctx context.Context, ref forge.ProjectRef, opts forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	if err := ref.Validate(); err != nil {
		return forge.ProjectItemPage{}, err
	}
	entry, err := ownerEntry(ref.OwnerKind)
	if err != nil {
		return forge.ProjectItemPage{}, err
	}
	per := opts.PerPage
	if per <= 0 || per > projectItemsPageSize {
		per = projectItemsPageSize
	}
	vars := map[string]any{
		"login": ref.Owner, "number": ref.Number,
		"first": per, "values": projectItemValuesPerItem,
	}
	if opts.Cursor != "" {
		vars["after"] = opts.Cursor
	}
	var out wireProjectFields
	q := fmt.Sprintf(projectItemsQuery, entry)
	if err := c.graphQLOp(ctx, "list project items", q, vars, &out); err != nil {
		return forge.ProjectItemPage{}, projectFromErr(err, ref)
	}
	p := out.project()
	if p == nil {
		return forge.ProjectItemPage{}, fmt.Errorf("%w: %s (%s)", forge.ErrProjectNotFound, ref.String(), ref.OwnerKind.OrDefault())
	}
	page := forge.ProjectItemPage{
		HasNext:    p.Items.PageInfo.HasNextPage,
		NextCursor: p.Items.PageInfo.EndCursor,
	}
	for _, n := range p.Items.Nodes {
		page.Items = append(page.Items, normalizeItem(n))
	}
	return page, nil
}

// IssueContentID resolves "owner/repo"#number to the issue's node id, the
// handle AddItem takes.
func (c *AdminClient) IssueContentID(ctx context.Context, repo string, number int) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("github: issue content id: repo must be owner/name, got %q", repo)
	}
	if number <= 0 {
		return "", fmt.Errorf("github: issue content id: issue number must be positive, got %d", number)
	}
	var out struct {
		Repository *struct {
			Issue *struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "name": name, "number": number}
	if err := c.graphQLOp(ctx, "issue node id", issueNodeIDQuery, vars, &out); err != nil {
		return "", err
	}
	if out.Repository == nil || out.Repository.Issue == nil || out.Repository.Issue.ID == "" {
		return "", fmt.Errorf("github: issue node id: %s#%d not found", repo, number)
	}
	return out.Repository.Issue.ID, nil
}

// AddItem puts a piece of content on the board. GitHub's mutation is
// idempotent: adding content already on the board returns the existing item.
func (c *AdminClient) AddItem(ctx context.Context, projectID, contentID string) (forge.ProjectItem, error) {
	if projectID == "" || contentID == "" {
		return forge.ProjectItem{}, errors.New("github: add project item: projectId and contentId are required")
	}
	var out struct {
		Add *struct {
			Item *wireItem `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	vars := map[string]any{"projectId": projectID, "contentId": contentID}
	if err := c.graphQLOp(ctx, "add project item", addProjectItemMutation, vars, &out); err != nil {
		return forge.ProjectItem{}, err
	}
	if out.Add == nil || out.Add.Item == nil || out.Add.Item.ID == "" {
		return forge.ProjectItem{}, errors.New("github: add project item: mutation returned no item")
	}
	return normalizeItem(*out.Add.Item), nil
}

// SetSingleSelect writes one single-select field value on one item — the ONLY
// write iterion performs on a board.
func (c *AdminClient) SetSingleSelect(ctx context.Context, projectID, itemID, fieldID, optionID string) error {
	if projectID == "" || itemID == "" || fieldID == "" || optionID == "" {
		return fmt.Errorf("github: set project field: projectId/itemId/fieldId/optionId are all required (got %q/%q/%q/%q)",
			projectID, itemID, fieldID, optionID)
	}
	var out struct {
		Update *struct {
			Item *struct {
				ID string `json:"id"`
			} `json:"projectV2Item"`
		} `json:"updateProjectV2ItemFieldValue"`
	}
	vars := map[string]any{"projectId": projectID, "itemId": itemID, "fieldId": fieldID, "optionId": optionID}
	if err := c.graphQLOp(ctx, "set project field", setSingleSelectMutation, vars, &out); err != nil {
		return err
	}
	if out.Update == nil || out.Update.Item == nil || out.Update.Item.ID == "" {
		return errors.New("github: set project field: mutation returned no item")
	}
	return nil
}
