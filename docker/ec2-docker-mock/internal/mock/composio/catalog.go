package composio

// The catalog is the single source of "limited": exactly the toolkits and tools
// this mock knows about. Everything the composio group serves — auth_configs,
// the global toolkit list, the tool list, tools/enum, and execute dispatch — is
// derived from these two slices, so adding a toolkit or tool is a one-place edit.

// toolkit is a mocked provider (gmail, googlesheets, …). Only OAUTH2,
// composio-managed auth is modeled; that is all toolkit-proxy exercises.
type toolkit struct {
	Slug        string
	Name        string
	Description string
}

// tool is one executable action. ReadOnly drives the `readonly`/`write` tag and
// the `scopes` entry the tool list exposes, so callers can filter read vs write.
type tool struct {
	Slug        string
	Name        string
	Description string
	Toolkit     string
	ReadOnly    bool
}

// toolkits is the fixed set of six mocked toolkits (github is intentionally
// absent — the stack routes it through gitana, not Composio).
var toolkits = []toolkit{
	{Slug: "gmail", Name: "Gmail", Description: "Mock Gmail — send, draft, and read messages."},
	{Slug: "googlesheets", Name: "Google Sheets", Description: "Mock Google Sheets — append, update, and read rows."},
	{Slug: "googledrive", Name: "Google Drive", Description: "Mock Google Drive — upload, list, and download files."},
	{Slug: "googlecalendar", Name: "Google Calendar", Description: "Mock Google Calendar — create and list events."},
	{Slug: "notion", Name: "Notion", Description: "Mock Notion — create, read, and search pages."},
	{Slug: "slack", Name: "Slack", Description: "Mock Slack — list channels and send messages."},
}

// tools is the fixed catalog, a deliberate read/write mix per toolkit.
var tools = []tool{
	// gmail
	{Slug: "GMAIL_FETCH_EMAILS", Name: "Fetch Emails", Description: "Lists messages in the mailbox.", Toolkit: "gmail", ReadOnly: true},
	{Slug: "GMAIL_GET_MESSAGE", Name: "Get Message", Description: "Fetches a single message by id.", Toolkit: "gmail", ReadOnly: true},
	{Slug: "GMAIL_SEND_EMAIL", Name: "Send Email", Description: "Sends an email (stored as a SENT message).", Toolkit: "gmail", ReadOnly: false},
	{Slug: "GMAIL_CREATE_DRAFT", Name: "Create Draft", Description: "Creates a draft message.", Toolkit: "gmail", ReadOnly: false},

	// googlesheets
	{Slug: "GOOGLESHEETS_GET_SPREADSHEET", Name: "Get Spreadsheet", Description: "Returns spreadsheet metadata.", Toolkit: "googlesheets", ReadOnly: true},
	{Slug: "GOOGLESHEETS_BATCH_GET", Name: "Batch Get", Description: "Reads all rows from the spreadsheet.", Toolkit: "googlesheets", ReadOnly: true},
	{Slug: "GOOGLESHEETS_APPEND_VALUES", Name: "Append Values", Description: "Appends rows to the spreadsheet.", Toolkit: "googlesheets", ReadOnly: false},
	{Slug: "GOOGLESHEETS_UPDATE_VALUES", Name: "Update Values", Description: "Overwrites the spreadsheet rows.", Toolkit: "googlesheets", ReadOnly: false},

	// googledrive
	{Slug: "GOOGLEDRIVE_LIST_FILES", Name: "List Files", Description: "Lists files and folders.", Toolkit: "googledrive", ReadOnly: true},
	{Slug: "GOOGLEDRIVE_DOWNLOAD_FILE", Name: "Download File", Description: "Returns a file's stored content.", Toolkit: "googledrive", ReadOnly: true},
	{Slug: "GOOGLEDRIVE_UPLOAD_FILE", Name: "Upload File", Description: "Stores a file.", Toolkit: "googledrive", ReadOnly: false},
	{Slug: "GOOGLEDRIVE_CREATE_FOLDER", Name: "Create Folder", Description: "Creates a folder.", Toolkit: "googledrive", ReadOnly: false},

	// googlecalendar
	{Slug: "GOOGLECALENDAR_LIST_EVENTS", Name: "List Events", Description: "Lists calendar events.", Toolkit: "googlecalendar", ReadOnly: true},
	{Slug: "GOOGLECALENDAR_CREATE_EVENT", Name: "Create Event", Description: "Creates a calendar event.", Toolkit: "googlecalendar", ReadOnly: false},

	// notion
	{Slug: "NOTION_GET_PAGE", Name: "Get Page", Description: "Fetches a page by id.", Toolkit: "notion", ReadOnly: true},
	{Slug: "NOTION_SEARCH", Name: "Search", Description: "Searches pages by title.", Toolkit: "notion", ReadOnly: true},
	{Slug: "NOTION_CREATE_PAGE", Name: "Create Page", Description: "Creates a page.", Toolkit: "notion", ReadOnly: false},

	// slack
	{Slug: "SLACK_LIST_CHANNELS", Name: "List Channels", Description: "Lists channels.", Toolkit: "slack", ReadOnly: true},
	{Slug: "SLACK_SEND_MESSAGE", Name: "Send Message", Description: "Posts a message to a channel.", Toolkit: "slack", ReadOnly: false},
}

// slackChannels are the fixed channels SLACK_LIST_CHANNELS returns; messages
// are appended per connect-ref under these names.
var slackChannels = []string{"general", "random", "dev"}

// logo returns the conventional Composio logo URL for a toolkit slug. Cosmetic —
// console renders it, nothing parses it.
func logo(slug string) string { return "https://logos.composio.dev/api/" + slug }

// toolkitBySlug returns the toolkit for a slug, or false if unknown.
func toolkitBySlug(slug string) (toolkit, bool) {
	for _, t := range toolkits {
		if t.Slug == slug {
			return t, true
		}
	}
	return toolkit{}, false
}

// toolBySlug returns the tool for a slug, or false if unknown.
func toolBySlug(slug string) (tool, bool) {
	for _, t := range tools {
		if t.Slug == slug {
			return t, true
		}
	}
	return tool{}, false
}

// tagsFor returns the tool-list tags for a tool: a single readonly/write tag.
func tagsFor(t tool) []string {
	if t.ReadOnly {
		return []string{"readonly"}
	}
	return []string{"write"}
}

// scopesFor returns a synthetic scope list mirroring the read/write split, so a
// `scopes=` filter behaves like the real API's coarse read vs write scoping.
func scopesFor(t tool) []string {
	if t.ReadOnly {
		return []string{t.Toolkit + ".readonly"}
	}
	return []string{t.Toolkit + ".write"}
}
