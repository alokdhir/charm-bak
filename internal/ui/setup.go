package ui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backends"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// setupFlow sets up a machine's repository, one decision per screen:
// where (a backend), its details, a check run on that machine (nothing
// there: create; a repository: its password connects), the recovery key
// when one was made, then on to choosing what to back up. It opens by
// itself when this machine has no repository, and from the Repository
// popup (o) for any machine without one.
type setupFlow struct {
	host   string
	remote bool
	step   int

	pick    int // the backend under the cursor on sfWhere
	backend backends.Backend
	inputs  []textinput.Model
	field   int
	// prefill is where another machine's repository is, for starting a
	// new one beside it.
	prefill map[string]map[string]string // by backend id

	location string
	env      map[string]string
	password textinput.Model // sfConnect

	busy string // what's running ("checking"), "" when idle
	err  error
	note string // a failed probe's reason, on sfDetails

	key    string // the recovery key, shown once on sfKey
	copied bool
	result *protocol.RepositoryInfo

	// S3's key: typed (keyTyped), or made by this Mac's daemon with its
	// key manager or after signing in to AWS in the browser.
	keyFrom int
	made    bool // the key fields hold a made key
	keyReq  int  // which makeKey a reply answers; Esc while waiting moves on
	awsDone int  // how many of the things being made in AWS are done
	howPick int
	aws     protocol.AWSStatusResult
}

const (
	keyTyped   = iota
	keyManager // the key manager makes it, no sign-in
	keySignIn  // aws login in the browser, then everything
)

const (
	sfWhere   = iota
	sfDetails // the backend's fields
	sfHow     // S3: make a key for me, or type one
	sfCreate  // checked: nothing there yet
	sfConnect // checked: a repository; its password connects
	sfKey     // the recovery key, once
	sfDone
)

// sfTrail is the step names along the top; sfCreate and sfConnect are
// both the Check.
var sfTrail = []string{"Where", "Details", "Check", "Key", "Done"}

func sfTrailIndex(step int) int {
	switch step {
	case sfWhere, sfHow:
		return 0
	case sfDetails:
		return 1
	case sfCreate, sfConnect:
		return 2
	case sfKey:
		return 3
	}
	return 4
}

type setupProbedMsg struct {
	res protocol.RepositoryProbeResult
	err error
}

type setupAWSMsg struct {
	res protocol.AWSStatusResult
	err error
}

type setupKeysMsg struct {
	req int
	res protocol.AWSKeysResult
	err error
}

type setupDoneMsg struct {
	res protocol.RepositorySetupResult
	err error
}

// startSetup opens the flow for host, starting from this machine's
// repository when host is another machine and this one has one: the
// same backend and bucket, host's own folder.
func (a *App) startSetup(host string) tea.Cmd {
	f := &setupFlow{host: host, remote: host != a.localHost(), prefill: map[string]map[string]string{}}
	if r := a.repoStatus.Repository; r != nil && f.remote {
		if id, v, ok := backends.Parse(r.Location); ok && id != "local" {
			b, _ := backends.ByID(id)
			f.prefill[id] = b.For(v, a.localHost(), host)
			for i, b := range backends.All {
				if b.ID == id {
					f.pick = i
				}
			}
		}
	}
	a.modal = f
	repo := a.repo
	return func() tea.Msg {
		res, err := repo.AWSStatus(context.Background())
		return setupAWSMsg{res, err}
	}
}

// noRepository reports whether host is known to have no repository yet:
// this machine from its status, a remote from the status fetched with its
// sources. Unknown (not answering, not loaded) is false.
func (a *App) noRepository(host string) bool {
	if host == a.localHost() {
		return a.statusKnown && a.repoStatus.Repository == nil
	}
	st, ok := a.hostStatus[host]
	return ok && st.Repository == nil && a.findHost(host).State == protocol.HostOK
}

func (f *setupFlow) typing() bool { return f.step == sfDetails || f.step == sfConnect }

// choose takes the picked backend: S3 asks how its key is made first,
// the rest go straight to their fields.
func (f *setupFlow) choose() tea.Cmd {
	f.prepare()
	if f.backend.ID == "s3" {
		f.step, f.err, f.note, f.howPick = sfHow, nil, "", 0
		return f.focus()
	}
	f.keyFrom = keyTyped
	return f.details()
}

// howOption is one way to get S3's key, on sfHow.
type howOption struct {
	from         int
	label, blurb string
	needsCLI     bool
}

func (f *setupFlow) howOptions() []howOption {
	var opts []howOption
	if m := f.aws.Manager; m != nil {
		opts = append(opts, howOption{keyManager, "Make " + f.host + " a key",
			"No sign-in. charm-bak's key above makes it: that key can only make keys for " + m.Bucket + ", and can't read any backups.", true})
	}
	if f.aws.Manager != nil {
		return append(opts, howOption{keyTyped, "I have a key", "An access key and secret you made in the AWS console.", false})
	}
	return append(opts,
		howOption{keySignIn, "Sign in to AWS",
			"In your browser. charm-bak makes the bucket if it isn't there, a key that can only reach " + f.host + "'s folder, and a limited key of its own so the next machine needs no sign-in. Your sign-in isn't kept.", true},
		howOption{keyTyped, "I have a key", "An access key and secret you made in the AWS console.", false})
}

// hidden reports whether field i is left out: a made key isn't typed,
// and its folder is the machine's name (the key reaches only that).
func (f *setupFlow) hidden(i int) bool {
	if f.keyFrom == keyTyped {
		return false
	}
	fl := f.backend.Fields[i]
	return fl.Env != "" || fl.Key == "folder"
}

// moveField moves the field cursor by d over the fields shown.
func (f *setupFlow) moveField(d int) {
	n := len(f.inputs)
	for i := 1; i <= n; i++ {
		j := ((f.field+d*i)%n + n) % n
		if !f.hidden(j) {
			f.field = j
			return
		}
	}
}

func (f *setupFlow) input(key string) *textinput.Model {
	for i, fl := range f.backend.Fields {
		if fl.Key == key {
			return &f.inputs[i]
		}
	}
	return nil
}

// details moves to sfDetails, the cursor on the first shown field that
// needs typing.
func (f *setupFlow) details() tea.Cmd {
	if m := f.aws.Manager; f.keyFrom == keyManager && m != nil {
		if in := f.input("bucket"); in != nil {
			in.SetValue(m.Bucket)
		}
		if in := f.input("region"); in != nil && m.Region != "us-east-1" {
			in.SetValue(m.Region)
		}
	}
	if f.keyFrom != keyTyped {
		if in := f.input("folder"); in != nil {
			in.SetValue(f.host)
		}
	}
	if in := f.input("bucket"); in != nil {
		in.Placeholder = "my-backups"
		if f.keyFrom == keySignIn {
			in.Placeholder = "charm-bak-<account number>"
		}
	}
	f.field = -1
	for i, in := range f.inputs {
		if !f.hidden(i) && in.Value() == "" && !f.optional(i) {
			f.field = i
			break
		}
	}
	if f.field < 0 {
		f.field = 0
		if f.hidden(0) {
			f.moveField(1)
		}
	}
	f.step, f.err, f.note = sfDetails, nil, ""
	return f.focus()
}

// optional: the field may stay empty. Signing in makes a bucket named
// for the account when none is given.
func (f *setupFlow) optional(i int) bool {
	return f.backend.Fields[i].Optional || (f.keyFrom == keySignIn && f.backend.Fields[i].Key == "bucket")
}

// prepare makes the picked backend's fields, filled from the prefill or
// their defaults; picking the same backend again keeps what was typed.
func (f *setupFlow) prepare() {
	b := backends.All[f.pick]
	if b.ID != f.backend.ID || f.inputs == nil {
		f.backend = b
		f.inputs = make([]textinput.Model, len(b.Fields))
		pre := f.prefill[b.ID]
		for i, fl := range b.Fields {
			v := fl.DefaultFor(f.host)
			if s, ok := pre[fl.Key]; ok {
				v = s
			}
			in := newTextField(v, strings.ReplaceAll(fl.Placeholder, "{host}", f.host))
			if fl.Secret {
				in.EchoMode = textinput.EchoPassword
			}
			f.inputs[i] = in
		}
	}
}

// back returns from the check to the fields as they were.
func (f *setupFlow) back() tea.Cmd {
	f.step, f.err, f.note = sfDetails, nil, ""
	return f.focus()
}

func (f *setupFlow) focus() tea.Cmd {
	for i := range f.inputs {
		f.inputs[i].Blur()
	}
	f.password.Blur()
	switch f.step {
	case sfDetails:
		in := &f.inputs[f.field]
		in.CursorEnd()
		return in.Focus()
	case sfConnect:
		return f.password.Focus()
	}
	return nil
}

func (f *setupFlow) values() map[string]string {
	v := map[string]string{}
	for i, fl := range f.backend.Fields {
		v[fl.Key] = f.inputs[i].Value()
	}
	return v
}

// check builds the location and asks the machine that will use it what's
// there. A required field left empty gets the cursor instead.
func (f *setupFlow) check(a *App) tea.Cmd {
	for i, fl := range f.backend.Fields {
		if !f.hidden(i) && strings.TrimSpace(f.inputs[i].Value()) == "" && !f.optional(i) {
			f.field = i
			f.note = fl.Label + " is needed."
			return f.focus()
		}
	}
	if f.keyFrom != keyTyped && !f.made {
		return f.makeKey(a)
	}
	loc, env, err := f.backend.Build(f.values())
	if err != nil {
		f.note = err.Error()
		return nil
	}
	f.location, f.env, f.busy, f.err, f.note = loc, env, "checking", nil, ""
	repo := a.repo.ForHost(f.host)
	p := protocol.RepositoryProbeParams{Location: loc, Env: env}
	return func() tea.Msg {
		res, err := repo.ProbeRepository(context.Background(), p)
		return setupProbedMsg{res, err}
	}
}

// makeKey asks this Mac's daemon for the machine's key: signing in to
// AWS in the browser first, or with the key manager.
func (f *setupFlow) makeKey(a *App) tea.Cmd {
	p := protocol.AWSKeysParams{Host: f.host, SignIn: f.keyFrom == keySignIn}
	if in := f.input("bucket"); in != nil {
		p.Bucket = strings.TrimSpace(in.Value())
	}
	if in := f.input("region"); in != nil {
		p.Region = strings.TrimSpace(in.Value())
	}
	f.busy, f.err, f.note, f.awsDone = "creating charm-bak-"+f.host, nil, "", 0
	if p.SignIn {
		f.busy = "sign in to AWS in your browser; charm-bak waits"
	}
	f.keyReq++
	req, repo := f.keyReq, a.repo
	return func() tea.Msg {
		res, err := repo.AWSKeys(context.Background(), p)
		return setupKeysMsg{req, res, err}
	}
}

// keysMade fills in what the daemon made (the bucket's name when it
// chose one, the key) and goes on to the check.
func (f *setupFlow) keysMade(a *App, res protocol.AWSKeysResult) tea.Cmd {
	if in := f.input("bucket"); in != nil {
		in.SetValue(res.Bucket)
	}
	if in := f.input("region"); in != nil && (in.Value() != "" || res.Region != "us-east-1") {
		in.SetValue(res.Region)
	}
	for i, fl := range f.backend.Fields {
		if v, ok := res.Env[fl.Env]; ok && fl.Env != "" {
			f.inputs[i].SetValue(v)
		}
	}
	f.made = true
	if f.keyFrom == keySignIn {
		f.aws.Manager = &protocol.AWSManager{Bucket: res.Bucket, Region: res.Region}
	}
	return f.check(a)
}

// setup creates the repository (create) or connects to the one there,
// through this machine's daemon, which keeps or sends the secrets.
func (f *setupFlow) setup(a *App, create bool) tea.Cmd {
	p := protocol.RepositorySetupParams{Host: f.host, Location: f.location, Env: f.env, Init: create}
	f.busy, f.err = "creating", nil
	if !create {
		p.Password, f.busy = f.password.Value(), "connecting"
	}
	repo := a.repo
	return func() tea.Msg {
		res, err := repo.SetupRepository(context.Background(), p)
		return setupDoneMsg{res, err}
	}
}

func (f *setupFlow) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case setupAWSMsg:
		if msg.err == nil {
			f.aws = msg.res
			f.howPick = 0 // the easiest way there is
		}
		return nil
	case setupKeysMsg:
		if msg.req != f.keyReq {
			return nil // given up on with Esc
		}
		f.busy = ""
		if msg.err != nil {
			f.err = msg.err
			return nil
		}
		return f.keysMade(a, msg.res)
	case setupProbedMsg:
		f.busy = ""
		switch {
		case msg.err != nil:
			f.err = msg.err
		case msg.res.State == protocol.ProbeEmpty:
			f.step = sfCreate
		case msg.res.State == protocol.ProbeFailed:
			f.note = msg.res.Message
		default: // a repository is there; the probe had no password for it
			f.step = sfConnect
			f.password = newTextField("", "")
			f.password.EchoMode = textinput.EchoPassword
		}
		return f.focus()
	case setupDoneMsg:
		f.busy = ""
		if msg.err != nil {
			f.err = msg.err
			return nil
		}
		f.result, f.key = msg.res.Repository, msg.res.Password
		f.step = sfDone
		if f.key != "" {
			f.step = sfKey
		}
		a.statusMsg = f.host + " backs up to " + f.backend.Label
		return tea.Batch(f.focus(), a.loadStatus(), a.loadSources())
	case tea.KeyPressMsg:
		if f.busy != "" {
			// Signing in waits on the person; Esc stops waiting (the
			// daemon gives up by itself later). A check or setup in
			// flight can't be called back.
			if f.step == sfDetails && !f.made && f.keyFrom != keyTyped && key.Matches(msg, keyEsc) {
				f.keyReq++
				f.busy = ""
			}
			return nil
		}
		return f.keys(a, msg)
	}
	return nil
}

func (f *setupFlow) keys(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch f.step {
	case sfWhere:
		switch {
		case key.Matches(msg, keyEsc):
			a.modal = nil
		case msg.String() == "up" || msg.String() == "k":
			f.pick = max(f.pick-1, 0)
		case msg.String() == "down" || msg.String() == "j":
			f.pick = min(f.pick+1, len(backends.All)-1)
		case key.Matches(msg, keyEnter), msg.String() == "right":
			return f.choose()
		}
		return nil

	case sfHow:
		opts := f.howOptions()
		f.howPick = min(f.howPick, len(opts)-1)
		switch {
		case key.Matches(msg, keyEsc):
			f.step, f.note, f.err = sfWhere, "", nil
		case msg.String() == "up" || msg.String() == "k":
			f.howPick, f.note = max(f.howPick-1, 0), ""
		case msg.String() == "down" || msg.String() == "j":
			f.howPick, f.note = min(f.howPick+1, len(opts)-1), ""
		case key.Matches(msg, keyEnter), msg.String() == "right":
			o := opts[f.howPick]
			if o.needsCLI && !f.aws.CLI {
				f.note = "This needs the AWS CLI: brew install awscli"
				return nil
			}
			if o.from != f.keyFrom {
				f.made = false
			}
			f.keyFrom = o.from
			return f.details()
		}
		return nil

	case sfDetails:
		switch {
		case key.Matches(msg, keyEsc):
			f.step, f.note, f.err = sfWhere, "", nil
			if f.backend.ID == "s3" {
				f.step = sfHow
			}
			return f.focus()
		case key.Matches(msg, keyEnter):
			return f.check(a)
		case key.Matches(msg, keyNext), msg.String() == "down":
			f.moveField(1)
			return f.focus()
		case key.Matches(msg, keyPrev), msg.String() == "up":
			f.moveField(-1)
			return f.focus()
		}
		var cmd tea.Cmd
		before := f.inputs[f.field].Value()
		f.inputs[f.field], cmd = f.inputs[f.field].Update(msg)
		if f.inputs[f.field].Value() != before {
			f.made = false // a key is made for a bucket; another needs another
		}
		f.note, f.err = "", nil
		return cmd

	case sfCreate:
		switch {
		case key.Matches(msg, keyEsc):
			return f.back()
		case key.Matches(msg, keyEnter):
			return f.setup(a, true)
		}
		return nil

	case sfConnect:
		switch {
		case key.Matches(msg, keyEsc):
			return f.back()
		case key.Matches(msg, keyEnter):
			if f.password.Value() == "" {
				return nil
			}
			return f.setup(a, false)
		}
		var cmd tea.Cmd
		f.password, cmd = f.password.Update(msg)
		f.err = nil
		return cmd

	case sfKey:
		switch {
		case msg.String() == "c":
			f.copied = true
			return tea.Batch(copyToClipboardCmd(f.key), audibleCueCmd())
		case key.Matches(msg, keyEnter):
			f.step = sfDone
		}
		return nil // no way back past a key that isn't saved

	case sfDone:
		switch {
		case key.Matches(msg, keyEsc):
			a.modal = nil
		case key.Matches(msg, keyEnter):
			a.modal = nil
			return a.startAddFor(f.host)
		}
	}
	return nil
}

// blurb is a backend's line in the list. A folder is on the machine
// being set up, which for a remote isn't this Mac: say whose.
func (f *setupFlow) blurb(be backends.Backend) string {
	if be.ID == "local" && f.remote {
		return "a folder or disk on " + f.host + ", not this Mac"
	}
	return be.Blurb
}

// where is the location as shown: a folder says which machine it's on.
func (f *setupFlow) where() string {
	if f.backend.ID == "local" {
		return f.location + " on " + f.host
	}
	return f.location
}

// keyAt reports whether screen cell (x, y) is on the recovery key's line,
// found by rendering the popup again where the view centers it.
func (f *setupFlow) keyAt(a *App, x, y int) bool {
	if f.step != sfKey || f.key == "" {
		return false
	}
	popup := f.view(a)
	pw, ph := lipgloss.Width(popup), lipgloss.Height(popup)
	top := heroHeight + actionHeight + max((a.bodyHeight()-ph)/2, 0)
	left := max((a.width-pw)/2, 0)
	lines := strings.Split(popup, "\n")
	i := y - top
	return x >= left && x < left+pw && i >= 0 && i < len(lines) && strings.Contains(ansi.Strip(lines[i]), f.key)
}

// view keeps one size from the first screen to the last: every step is
// padded to the tallest of the backend list, this backend's details and
// the step itself, so moving through the flow never reshapes the popup.
func (f *setupFlow) view(a *App) string {
	inner := popupInnerWidth(a.width)
	height := func(step int) int {
		return lipgloss.Height(lipgloss.NewStyle().Width(inner).Render(f.body(a, step)))
	}
	body := f.body(a, f.step)
	tallest := max(height(sfWhere), height(f.step))
	// S3's screens are the tallest there are; measured from the start
	// so picking it doesn't grow the popup.
	s3 := *f
	for i, be := range backends.All {
		if be.ID == "s3" {
			s3.pick, s3.inputs = i, nil
			s3.prepare()
			// The copy keeps f's cursor, which may be past S3's last
			// field (another backend has more).
			s3.field = min(s3.field, len(be.Fields)-1)
			h := func(step int) int {
				return lipgloss.Height(lipgloss.NewStyle().Width(inner).Render(s3.body(a, step)))
			}
			// With no bucket name and with the longest there can be (63
			// characters), so typing one never grows the popup.
			for _, bucket := range []string{"", strings.Repeat("x", 63)} {
				s3.input("bucket").SetValue(bucket)
				for _, k := range []int{keyTyped, keyManager, keySignIn} {
					s3.keyFrom = k
					tallest = max(tallest, h(sfDetails))
				}
			}
			tallest = max(tallest, h(sfHow))
			// The recovery key's screen, with the longest place a copy
			// can be kept.
			s3.key = strings.Repeat("x", 44)
			s3.result = &protocol.RepositoryInfo{PasswordFrom: "file /Library/Application Support/charm-bak/state/repo-password"}
			tallest = max(tallest, h(sfKey))
		}
	}
	if f.inputs != nil {
		// Measured with every field shown, so a made key's shorter
		// form doesn't shrink the popup.
		from := f.keyFrom
		for _, k := range []int{keyTyped, keyManager, keySignIn} {
			f.keyFrom = k
			tallest = max(tallest, height(sfDetails))
		}
		f.keyFrom = from
		if f.backend.ID == "s3" {
			tallest = max(tallest, height(sfHow))
		}
	}
	// Never taller than the screen has room for (the border and the
	// footer's three lines): a small terminal gives up the room kept for
	// the longest bucket names before it gives up fitting.
	tallest = max(min(tallest, max(a.bodyHeight()-5, height(sfWhere))), height(f.step))
	if pad := tallest - height(f.step); pad > 0 {
		body += strings.Repeat("\n", pad)
	}
	return boxed(body+f.footer(a), inner)
}

// trail is the title and the steps, the current one lit.
func (f *setupFlow) trail(inner int) string {
	title := popupTitle("Set up " + f.host)
	cur := sfTrailIndex(f.step)
	var parts []string
	for i, s := range sfTrail {
		switch {
		case i == cur:
			parts = append(parts, styles.HotkeyStyle().Render(s))
		case i < cur:
			parts = append(parts, styles.FieldValueStyle().Render(s))
		default:
			parts = append(parts, styles.MutedStyle().Render(s))
		}
	}
	steps := strings.Join(parts, styles.MutedStyle().Render(" › "))
	gap := inner - lipgloss.Width(title) - lipgloss.Width(steps)
	if gap < 2 {
		return title
	}
	return title + strings.Repeat(" ", gap) + steps
}

func (f *setupFlow) body(a *App, step int) string {
	inner := popupInnerWidth(a.width)
	vw := inner - formLabelWidth
	text := func(s string) string { return styles.FieldValueStyle().Width(inner).Render(s) }
	muted := func(s string) string { return styles.MutedStyle().Width(inner).Render(s) }
	var b formBuilder
	b.WriteString(f.trail(inner) + "\n\n")

	switch step {
	case sfWhere:
		b.WriteString(text("Where should "+f.host+"'s backups go?") + "\n\n")
		for i, be := range backends.All {
			label := "  " + be.Label
			if i == f.pick {
				label = styles.SelectedStyle().Render("▸ " + be.Label)
			} else {
				label = styles.FieldValueStyle().Render(label)
			}
			b.Row(label, styles.MutedStyle().Render(midTrunc(f.blurb(be), max(vw-8, 10))))
		}

	case sfHow:
		b.WriteString(f.head() + "\n\n")
		if m := f.aws.Manager; m != nil {
			// Not a sign-in: a stored key that can only make keys.
			acct := ""
			if m.Account != "" {
				acct = ", account " + m.Account
			}
			b.Row(styles.FieldLabelStyle().Render("AWS"), styles.HealthStyle(styles.OK).Render("✓ connected")+styles.MutedStyle().Render("  through charm-bak's own limited key"+acct))
			b.WriteString("\n")
		}
		b.WriteString(text("How should "+f.host+" get its key?") + "\n\n")
		for i, o := range f.howOptions() {
			label := styles.FieldValueStyle().Render("  " + o.label)
			if i == f.howPick {
				label = styles.SelectedStyle().Render("▸ " + o.label)
			}
			blurb := o.blurb
			if o.needsCLI && !f.aws.CLI {
				blurb = "Needs the AWS CLI (brew install awscli). " + blurb
			}
			b.WriteString(label + "\n" + styles.MutedStyle().Width(inner).PaddingLeft(4).Render(blurb) + "\n")
		}

	case sfDetails:
		be := f.backend
		b.WriteString(f.head() + "\n\n")
		for i, fl := range be.Fields {
			if f.hidden(i) {
				continue
			}
			lab := styles.FieldLabelStyle()
			if i == f.field {
				lab = styles.HotkeyStyle()
			}
			f.inputs[i].SetWidth(vw - 1)
			b.Row(lab.Render(fl.Label), f.inputs[i].View())
		}
		help := be.Fields[f.field].Help
		if be.ID == "local" && f.remote {
			help = "A folder on " + f.host + ", made if it isn't there. A second disk or a mounted share, not the disk being backed up."
		}
		if f.optional(f.field) && help == "" {
			help = "optional"
		}
		if f.keyFrom != keyTyped {
			// What will be made in the person's AWS account, all of it,
			// before Enter does it.
			intro, items, outro := f.creates()
			b.WriteString("\n" + text(intro) + "\n" + bullets(items, f.awsDone, inner, styles.FieldValueStyle()) + "\n")
			if outro != "" {
				b.WriteString(text(outro) + "\n")
			}
			break
		}
		b.WriteString("\n" + muted(help) + "\n")
		if be.Note != "" {
			b.WriteString(muted(be.Note) + "\n")
		}

	case sfCreate:
		b.WriteString(styles.HealthStyle(styles.OK).Render("✓ ") + styles.FieldValueStyle().Render("Reached it. Nothing here yet.") + "\n\n")
		b.Row(styles.FieldLabelStyle().Render("Location"), styles.FieldValueStyle().Render(midTrunc(f.where(), vw)))
		b.WriteString("\n" + text("Enter creates "+f.host+"'s repository there. charm-bak makes its password and shows it to you once.") + "\n")

	case sfConnect:
		b.WriteString(text("There's a repository here already.") + "\n\n")
		b.Row(styles.FieldLabelStyle().Render("Location"), styles.FieldValueStyle().Render(midTrunc(f.where(), vw)))
		f.password.SetWidth(vw - 1)
		b.Row(styles.HotkeyStyle().Render("Password"), f.password.View())
		b.WriteString("\n" + muted("Its password connects "+f.host+" to it, with the backups already there.") + "\n")

	case sfKey:
		b.WriteString(text("Your recovery key") + "\n\n")
		b.WriteString("  " + styles.FileTitleStyle().Render(f.key) + "\n\n")
		b.WriteString(text("Backups are encrypted with it. charm-bak saved it, so there's nothing you have to do for backups to work:") + "\n")
		b.WriteString(bullets(f.keptAt(), 0, inner, styles.FieldValueStyle()) + "\n")
		b.WriteString(text("If those are lost, it's the only way back to the backups. Keep a copy in your password manager.") + "\n")

	case sfDone:
		loc := f.where()
		b.WriteString(styles.HealthStyle(styles.OK).Render("✓ ") + styles.FieldValueStyle().Render(f.host+" backs up to "+f.backend.Label) + "\n")
		b.WriteString(muted(midTrunc(loc, inner)) + "\n\n")
		b.WriteString(text("Next: choose what to back up.") + "\n")
	}
	return b.String()
}

// creates says what Enter will make in the AWS account, every thing by
// name: an opening line, the things, a closing line ("" for none).
func (f *setupFlow) creates() (string, []string, string) {
	bucket := ""
	if in := f.input("bucket"); in != nil {
		bucket = strings.TrimSpace(in.Value())
	}
	if bucket == "" {
		bucket = "charm-bak-<account number>"
	}
	user := "IAM user charm-bak-" + f.host + ": reaches only " + bucket + "/" + f.host + "/"
	if f.keyFrom == keyManager {
		return "Enter creates in your AWS account:", []string{user}, ""
	}
	// In the order the daemon makes them, so they tick off in order.
	return "Enter signs in to AWS in your browser (the sign-in isn't kept), then creates:",
		[]string{
			"Bucket " + bucket + ", if it isn't there (private, encrypted)",
			"IAM user charm-bak-manager, policy charm-bak-boundary-" + bucket + ": they only make users like the next one, so the next machine needs no sign-in",
			user,
		},
		""
}

// bullets renders items with a hanging indent, wrapped at width; the
// first done are ticked.
func bullets(items []string, done, width int, st lipgloss.Style) string {
	var out []string
	for n, it := range items {
		lines := strings.Split(st.Width(width-2).Render(it), "\n")
		for i, l := range lines {
			lead := st.Render("  ")
			if i == 0 {
				lead = st.Render("• ")
				if n < done {
					lead = styles.HealthStyle(styles.OK).Render("✓ ")
				}
			}
			out = append(out, lead+l)
		}
	}
	return strings.Join(out, "\n")
}

// awsStep follows the daemon through making the key: the things made so
// far ticked, the busy line saying what it's doing now.
func (f *setupFlow) awsStep(e *protocol.AWSStepEvent) {
	if e.For != f.host || f.busy == "" || f.made {
		return
	}
	signIn := f.keyFrom == keySignIn
	switch e.Step {
	case protocol.AWSStepSignedIn:
		f.busy = "signed in to AWS"
		if e.Account != "" {
			f.busy += " (account " + e.Account + ")"
		}
		f.busy += "; setting up the bucket"
	case protocol.AWSStepBucket:
		f.awsDone, f.busy = 1, "creating charm-bak-manager"
	case protocol.AWSStepManager:
		f.awsDone, f.busy = 2, "creating charm-bak-"+f.host
	case protocol.AWSStepUser:
		f.awsDone, f.busy = 1, "waiting for AWS to accept "+f.host+"'s new key, usually a few seconds"
		if signIn {
			f.awsDone = 3
		}
	}
}

// keptAt says where charm-bak saved the recovery key: this Mac's
// Keychain always, and for a remote its own copy, where the remote
// reported it ("file /…/repo-password").
func (f *setupFlow) keptAt() []string {
	at := []string{"This Mac's Keychain: item charm-bak-repo, account " + f.host + " (open Keychain Access and search charm-bak)"}
	if f.remote {
		where := "a file only root can read"
		if f.result != nil {
			if p, ok := strings.CutPrefix(f.result.PasswordFrom, "file "); ok {
				where = p + " (only root can read it)"
			}
		}
		at = append(at, "On "+f.host+": "+where)
	}
	return at
}

// head is the backend's name.
func (f *setupFlow) head() string {
	return styles.FileTitleStyle().Render(f.backend.Label)
}

// footer is the status line (what's running, or what went wrong) and the
// step's keys.
func (f *setupFlow) footer(a *App) string {
	var b strings.Builder
	b.WriteString("\n")
	switch {
	case f.busy != "":
		where := ""
		if f.busy == "checking" {
			where = " from " + f.host
		}
		b.WriteString(styles.HealthStyle(styles.Warn).Render(a.spinFrame()) + " " + styles.MutedStyle().Render(f.busy+where+"…") + "\n")
	case f.err != nil:
		b.WriteString(errorLine(f.err) + "\n")
	case f.note != "":
		b.WriteString(styles.HealthStyle(styles.Fail).Render(f.note) + "\n")
	case f.copied && f.step == sfKey:
		b.WriteString(styles.MutedStyle().Render("copied") + "\n")
	default:
		b.WriteString("\n")
	}
	switch f.step {
	case sfWhere:
		b.WriteString(hints("↑↓", "choose", "enter", "next", "esc", "later"))
	case sfHow:
		b.WriteString(hints("↑↓", "choose", "enter", "next", "esc", "back"))
	case sfDetails:
		switch f.keyFrom {
		case keySignIn:
			b.WriteString(hints("tab", "field", "enter", "sign in", "esc", "back"))
		case keyManager:
			b.WriteString(hints("tab", "field", "enter", "make key", "esc", "back"))
		default:
			b.WriteString(hints("tab", "field", "enter", "check", "esc", "back"))
		}
	case sfCreate:
		b.WriteString(hints("enter", "create", "esc", "back"))
	case sfConnect:
		b.WriteString(hints("enter", "connect", "esc", "back"))
	case sfKey:
		b.WriteString(hints("c or click", "copy", "enter", "I've saved it"))
	case sfDone:
		b.WriteString(hints("enter", "choose folders", "esc", "later"))
	}
	return b.String()
}
