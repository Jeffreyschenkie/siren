package main

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bcmk/go-smtpd/smtpd"
	"github.com/bcmk/siren/lib"
	"github.com/bcmk/siren/payments"
	tg "github.com/bcmk/telegram-bot-api"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

var (
	version  = "5.0"
	checkErr = lib.CheckErr
	lerr     = lib.Lerr
	linf     = lib.Linf
	ldbg     = lib.Ldbg
)

type worker struct {
	clients         []*lib.Client
	bots            map[string]*tg.BotAPI
	db              *sql.DB
	cfg             *config
	elapsed         time.Duration
	tr              map[string]translations
	checkModel      func(client *lib.Client, modelID string, headers [][2]string, dbg bool) lib.StatusKind
	startChecker    func(usersOnlineEndpoint string, clients []*lib.Client, headers [][2]string, intervalMs int, debug bool) (input chan []string, output chan lib.StatusUpdate, elapsed chan time.Duration)
	senders         map[string]func(msg tg.Chattable) (tg.Message, error)
	unknowns        []bool
	unknownsPos     int
	nextErrorReport time.Time
	coinPaymentsAPI *payments.CoinPaymentsAPI
	ipnServeMux     *http.ServeMux
	mailTLS         *tls.Config
}

type packet struct {
	message  tg.Update
	endpoint string
}

type email struct {
	chatID   int64
	endpoint string
	email    string
}

type appliedKind int

const (
	invalidReferral appliedKind = iota
	followerExists
	referralApplied
)

func newWorker() *worker {
	if len(os.Args) != 2 {
		panic("usage: siren <config>")
	}
	cfg := readConfig(os.Args[1])

	var err error
	var mailTLS *tls.Config

	if cfg.Mail != nil {
		mailTLS, err = loadTLS(cfg.Mail.Certificate, cfg.Mail.CertificateKey)
		checkErr(err)
	}

	var clients []*lib.Client
	for _, address := range cfg.SourceIPAddresses {
		clients = append(clients, lib.HTTPClientWithTimeoutAndAddress(cfg.TimeoutSeconds, address, cfg.EnableCookies))
	}

	bots := make(map[string]*tg.BotAPI)
	senders := make(map[string]func(msg tg.Chattable) (tg.Message, error))
	for n, p := range cfg.Endpoints {
		//noinspection GoNilness
		bot, err := tg.NewBotAPIWithClient(p.BotToken, clients[0].Client)
		checkErr(err)
		bots[n] = bot
		senders[n] = bot.Send
	}
	db, err := sql.Open("sqlite3", cfg.DBPath)
	checkErr(err)
	w := &worker{
		bots:        bots,
		db:          db,
		cfg:         cfg,
		clients:     clients,
		tr:          loadAllTranslations(cfg),
		senders:     senders,
		unknowns:    make([]bool, cfg.errorDenominator),
		ipnServeMux: http.NewServeMux(),
		mailTLS:     mailTLS,
	}

	if cp := cfg.CoinPayments; cp != nil {
		w.coinPaymentsAPI = payments.NewCoinPaymentsAPI(cp.PublicKey, cp.PrivateKey, "https://"+cp.IPNListenURL, cfg.TimeoutSeconds, cfg.Debug)
	}

	switch cfg.Website {
	case "bongacams":
		w.checkModel = lib.CheckModelBongaCams
		switch w.cfg.Checker {
		case "api":
			w.startChecker = lib.StartBongaCamsAPIChecker
		case "redir":
			w.startChecker = lib.StartBongaCamsRedirChecker
		default:
			panic("specify checker")
		}
	case "chaturbate":
		w.checkModel = lib.CheckModelChaturbate
		w.startChecker = lib.StartChaturbateAPIChecker
	case "stripchat":
		w.checkModel = lib.CheckModelStripchat
		w.startChecker = lib.StartStripchatAPIChecker
	default:
		panic("wrong website")
	}

	return w
}

func (w *worker) setWebhook() {
	for n, p := range w.cfg.Endpoints {
		linf("setting webhook for endpoint %s...", n)
		if p.WebhookDomain == "" {
			continue
		}
		if p.CertificatePath == "" {
			var _, err = w.bots[n].SetWebhook(tg.NewWebhook(path.Join(p.WebhookDomain, p.ListenPath)))
			checkErr(err)
		} else {
			var _, err = w.bots[n].SetWebhook(tg.NewWebhookWithCert(path.Join(p.WebhookDomain, p.ListenPath), p.CertificatePath))
			checkErr(err)
		}
		info, err := w.bots[n].GetWebhookInfo()
		checkErr(err)
		if info.LastErrorDate != 0 {
			linf("last webhook error time: %v", time.Unix(int64(info.LastErrorDate), 0))
		}
		if info.LastErrorMessage != "" {
			linf("last webhook error message: %s", info.LastErrorMessage)
		}
		linf("OK")
	}

}

func (w *worker) removeWebhook() {
	for n := range w.cfg.Endpoints {
		linf("removing webhook for endpoint %s...", n)
		_, err := w.bots[n].RemoveWebhook()
		checkErr(err)
		linf("OK")
	}
}

func (w *worker) mustExec(query string, args ...interface{}) {
	stmt, err := w.db.Prepare(query)
	checkErr(err)
	_, err = stmt.Exec(args...)
	checkErr(err)
	checkErr(stmt.Close())
}

func (w *worker) incrementBlock(endpoint string, chatID int64) {
	w.mustExec("insert or ignore into block (endpoint, chat_id, block) values (?,?,?)", endpoint, chatID, 0)
	w.mustExec("update block set block=block+1 where chat_id=? and endpoint=?", chatID, endpoint)
}

func (w *worker) resetBlock(endpoint string, chatID int64) {
	w.mustExec("update block set block=0 where endpoint=? and chat_id=?", endpoint, chatID)
}

func (w *worker) sendText(endpoint string, chatID int64, notify bool, disablePreview bool, parse parseKind, text string) {
	msg := tg.NewMessage(chatID, text)
	msg.DisableNotification = !notify
	switch parse {
	case parseHTML, parseMarkdown:
		msg.ParseMode = parse.String()
	}
	w.sendMessage(endpoint, &messageConfig{msg})
}

func (w *worker) sendMessage(endpoint string, msg baseChattable) {
	chatID := msg.baseChat().ChatID
	if _, err := w.bots[endpoint].Send(msg); err != nil {
		switch err := err.(type) {
		case tg.Error:
			if err.Code == 403 {
				linf("bot is blocked by the user %d, %v", chatID, err)
				w.incrementBlock(endpoint, chatID)
			} else {
				lerr("cannot send a message to %d, code %d, %v", chatID, err.Code, err)
			}
		default:
			lerr("unexpected error type while sending a message to %d, %v", msg.baseChat().ChatID, err)
		}
		return
	}
	if w.cfg.Debug {
		ldbg("message sent to %d", chatID)
	}
	w.resetBlock(endpoint, chatID)
}

func (w *worker) sendTr(endpoint string, chatID int64, notify bool, translation *translation, args ...interface{}) {
	text := fmt.Sprintf(translation.Str, args...)
	w.sendText(endpoint, chatID, notify, translation.DisablePreview, translation.Parse, text)
}

func (w *worker) createDatabase() {
	linf("creating database if needed...")
	w.mustExec(`
		create table if not exists signals (
			chat_id integer,
			model_id text,
			endpoint text not null default '',
			primary key (chat_id, model_id, endpoint));`)
	w.mustExec(`
		create table if not exists statuses (
			model_id text primary key,
			status integer,
			not_found integer not null default 0,
			last_online integer not null default 0);`)
	w.mustExec(`
		create table if not exists models (
			model_id text primary key,
			referred_users integer not null default 0);`)
	w.mustExec(`
		create table if not exists feedback (
			chat_id integer,
			text text,
			endpoint text not null default '');`)
	w.mustExec(`
		create table if not exists block (
			chat_id integer,
			block integer not null default 0,
			endpoint text not null default '',
			primary key(chat_id, endpoint));`)
	w.mustExec(`
		create table if not exists users (
			chat_id integer primary key,
			max_models integer not null default 0,
			reports integer not null default 0);`)
	w.mustExec(`
		create table if not exists emails (
			chat_id integer,
			endpoint text not null default '',
			email text not null default '',
			primary key(chat_id, endpoint));`)
	w.mustExec(`
		create table if not exists transactions (
			local_id text primary key,
			kind text,
			chat_id integer,
			remote_id text,
			timeout integer,
			amount text,
			address string,
			status_url string,
			checkout_url string,
			dest_tag string,
			status integer,
			timestamp integer,
			model_number integer,
			currency text,
			endpoint text not null default ''
		);`)
	w.mustExec(`
		create table if not exists referrals (
			chat_id integer primary key,
			referral_id text not null default '',
			referred_users integer not null default 0);`)
}

func (w *worker) updateStatus(modelID string, newStatus lib.StatusKind, timestamp int) (notify bool) {
	if newStatus != lib.StatusNotFound {
		w.mustExec("update statuses set not_found=0 where model_id=?", modelID)
	} else {
		newStatus = lib.StatusOffline
	}

	signalsQuery := w.db.QueryRow("select count(*) from signals where model_id=?", modelID)
	if singleInt(signalsQuery) == 0 {
		return false
	}
	oldStatusQuery, err := w.db.Query("select status, last_online from statuses where model_id=?", modelID)
	checkErr(err)
	defer func() { checkErr(oldStatusQuery.Close()) }()
	if !oldStatusQuery.Next() {
		lastOnline := 0
		if newStatus == lib.StatusOnline {
			lastOnline = timestamp
		}
		w.mustExec("insert into statuses (model_id, status, last_online) values (?,?,?)", modelID, newStatus, lastOnline)
		return true
	}
	var oldStatus lib.StatusKind
	var lastOnline int
	checkErr(oldStatusQuery.Scan(&oldStatus, &lastOnline))
	checkErr(oldStatusQuery.Close())
	changeOK := w.cfg.OfflineNotifications || (oldStatus != lib.StatusOnline && newStatus == lib.StatusOnline)
	durationOK := w.cfg.OfflineNotifications || timestamp-lastOnline >= w.cfg.OfflineThresholdSeconds
	notify = oldStatus != newStatus && changeOK && durationOK
	if newStatus == lib.StatusOnline {
		lastOnline = timestamp
	}
	w.mustExec("update statuses set status=?, last_online=? where model_id=?", newStatus, lastOnline, modelID)
	return
}

func (w *worker) notFound(modelID string) bool {
	linf("model %s not found", modelID)
	exists := w.db.QueryRow("select count(*) from statuses where model_id=?", modelID)
	if singleInt(exists) == 0 {
		return false
	}
	w.mustExec("update statuses set not_found=not_found+1 where model_id=?", modelID)
	notFound := w.db.QueryRow("select not_found from statuses where model_id=?", modelID)
	return singleInt(notFound) > w.cfg.NotFoundThreshold
}

func (w *worker) reportNotFound(modelID string) {
	chats, endpoints := w.chatsForModel(modelID)
	for i, chatID := range chats {
		endpoint := endpoints[i]
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].ProfileRemoved, modelID)
	}
}

func (w *worker) removeNotFound(modelID string) {
	w.mustExec("delete from signals where model_id=?", modelID)
	w.cleanStatuses()
}

func (w *worker) models() (models []string) {
	modelsQuery, err := w.db.Query(
		`select distinct model_id from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where block.block is null or block.block<?
		order by model_id`,
		w.cfg.BlockThreshold)
	checkErr(err)
	defer func() { checkErr(modelsQuery.Close()) }()
	for modelsQuery.Next() {
		var modelID string
		checkErr(modelsQuery.Scan(&modelID))
		models = append(models, modelID)
	}
	return
}

func (w *worker) chatsForModel(modelID string) (chats []int64, endpoints []string) {
	chatsQuery, err := w.db.Query(
		`select signals.chat_id, signals.endpoint from signals left join block
		on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where signals.model_id=? and (block.block is null or block.block<?)
		order by signals.chat_id`,
		modelID,
		w.cfg.BlockThreshold)
	checkErr(err)
	defer func() { checkErr(chatsQuery.Close()) }()
	for chatsQuery.Next() {
		var chatID int64
		var endpoint string
		checkErr(chatsQuery.Scan(&chatID, &endpoint))
		chats = append(chats, chatID)
		endpoints = append(endpoints, endpoint)
	}
	return
}

func (w *worker) broadcastChats(endpoint string) (chats []int64) {
	chatsQuery, err := w.db.Query(
		`select distinct signals.chat_id from signals left join block
		on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block<?) and signals.endpoint=?
		order by signals.chat_id`,
		w.cfg.BlockThreshold,
		endpoint)
	checkErr(err)
	defer func() { checkErr(chatsQuery.Close()) }()
	for chatsQuery.Next() {
		var chatID int64
		checkErr(chatsQuery.Scan(&chatID))
		chats = append(chats, chatID)
	}
	return
}

func (w *worker) statusesForChat(endpoint string, chatID int64) []lib.StatusUpdate {
	statusesQuery, err := w.db.Query(`select statuses.model_id, statuses.status
		from statuses inner join signals
		on statuses.model_id=signals.model_id
		where signals.chat_id=? and signals.endpoint=?
		order by statuses.model_id`, chatID, endpoint)
	checkErr(err)
	defer func() { checkErr(statusesQuery.Close()) }()
	var statuses []lib.StatusUpdate
	for statusesQuery.Next() {
		var modelID string
		var status lib.StatusKind
		checkErr(statusesQuery.Scan(&modelID, &status))
		statuses = append(statuses, lib.StatusUpdate{ModelID: modelID, Status: status})
	}
	return statuses
}

func (w *worker) statusKey(endpoint string, status lib.StatusKind) *translation {
	switch status {
	case lib.StatusOnline:
		return w.tr[endpoint].OnlineList
	case lib.StatusDenied:
		return w.tr[endpoint].DeniedList
	default:
		return w.tr[endpoint].OfflineList
	}
}

func (w *worker) reportStatus(endpoint string, chatID int64, modelID string, status lib.StatusKind) {
	switch status {
	case lib.StatusOnline:
		w.sendTr(endpoint, chatID, true, w.tr[endpoint].Online, modelID)
	case lib.StatusOffline:
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Offline, modelID)
	case lib.StatusDenied:
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Denied, modelID)
	}
	w.addUser(endpoint, chatID)
	w.mustExec("update users set reports=reports+1 where chat_id=?", chatID)
}

func singleInt(row *sql.Row) (result int) {
	checkErr(row.Scan(&result))
	return result
}

func (w *worker) subscriptionExists(endpoint string, chatID int64, modelID string) bool {
	duplicate := w.db.QueryRow("select count(*) from signals where chat_id=? and model_id=? and endpoint=?", chatID, modelID, endpoint)
	count := singleInt(duplicate)
	return count != 0
}

func (w *worker) userExists(chatID int64) bool {
	count := singleInt(w.db.QueryRow("select count(*) from users where chat_id=?", chatID))
	return count != 0
}

func (w *worker) subscriptionsNumber(endpoint string, chatID int64) int {
	return singleInt(w.db.QueryRow("select count(*) from signals where chat_id=? and endpoint=?", chatID, endpoint))
}

func (w *worker) maxModels(chatID int64) int {
	query, err := w.db.Query("select max_models from users where chat_id=?", chatID)
	checkErr(err)
	defer func() { checkErr(query.Close()) }()
	if !query.Next() {
		return w.cfg.MaxModels
	}
	var result int
	checkErr(query.Scan(&result))
	return result
}

func (w *worker) addUser(endpoint string, chatID int64) {
	w.mustExec(`insert or ignore into users (chat_id, max_models) values (?, ?)`, chatID, w.cfg.MaxModels)
	w.mustExec(`insert or ignore into emails (endpoint, chat_id, email) values (?, ?, ?)`, endpoint, chatID, uuid.New())
}

func (w *worker) addModel(endpoint string, chatID int64, modelID string) bool {
	if modelID == "" {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].SyntaxAdd)
		return false
	}
	modelID = strings.ToLower(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, modelID)
		return false
	}

	w.addUser(endpoint, chatID)

	if w.subscriptionExists(endpoint, chatID, modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].AlreadyAdded, modelID)
		return false
	}
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	maxModels := w.maxModels(chatID)
	if subscriptionsNumber >= maxModels {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].NotEnoughSubscriptions)
		w.subscriptionUsage(endpoint, chatID, true)
		return false
	}
	status := w.modelActiveStatus(modelID)
	if status == lib.StatusUnknown {
		status = w.checkModel(w.clients[0], modelID, w.cfg.Headers, w.cfg.Debug)
	}
	if status == lib.StatusUnknown || status == lib.StatusNotFound {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].AddError, modelID)
		return false
	}
	w.mustExec("insert into signals (chat_id, model_id, endpoint) values (?,?,?)", chatID, modelID, endpoint)
	subscriptionsNumber++
	timestamp := int(time.Now().Unix())
	if status != lib.StatusExists {
		w.updateStatus(modelID, status, timestamp)
	}
	if status != lib.StatusDenied {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].ModelAdded, modelID)
	}
	if status != lib.StatusExists {
		w.reportStatus(endpoint, chatID, modelID, status)
	}
	if subscriptionsNumber >= maxModels-w.cfg.HeavyUserRemainder {
		w.subscriptionUsage(endpoint, chatID, true)
	}
	return true
}

func (w *worker) subscriptionUsage(endpoint string, chatID int64, ad bool) {
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	maxModels := w.maxModels(chatID)
	tr := w.tr[endpoint].SubscriptionUsage
	if ad {
		tr = w.tr[endpoint].SubscriptionUsageAd
	}
	w.sendTr(endpoint, chatID, false, tr, subscriptionsNumber, maxModels)
}

func (w *worker) wantMore(endpoint string, chatID int64) {
	w.subscriptionUsage(endpoint, chatID, false)
	w.showReferral(endpoint, chatID)

	if w.cfg.CoinPayments == nil || w.cfg.Mail == nil {
		return
	}

	text := fmt.Sprintf(w.tr[endpoint].BuyAd.Str,
		w.cfg.CoinPayments.subscriptionPacketPrice,
		w.cfg.CoinPayments.subscriptionPacketModelNumber)

	buttonText := fmt.Sprintf(w.tr[endpoint].BuyButton.Str, w.cfg.CoinPayments.subscriptionPacketModelNumber)
	buttons := [][]tg.InlineKeyboardButton{{tg.NewInlineKeyboardButtonData(buttonText, "buy")}}
	keyboard := tg.NewInlineKeyboardMarkup(buttons...)
	msg := tg.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	w.sendMessage(endpoint, &messageConfig{msg})
}

func (w *worker) removeModel(endpoint string, chatID int64, modelID string) {
	if modelID == "" {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].SyntaxRemove)
		return
	}
	modelID = strings.ToLower(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, modelID)
		return
	}
	if !w.subscriptionExists(endpoint, chatID, modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].ModelNotInList, modelID)
		return
	}
	w.mustExec("delete from signals where chat_id=? and model_id=? and endpoint=?", chatID, modelID, endpoint)
	w.cleanStatuses()
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].ModelRemoved, modelID)
}

func (w *worker) sureRemoveAll(endpoint string, chatID int64) {
	w.mustExec("delete from signals where chat_id=? and endpoint=?", chatID, endpoint)
	w.cleanStatuses()
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].AllModelsRemoved)
}

func (w *worker) buy(endpoint string, chatID int64) {
	var buttons [][]tg.InlineKeyboardButton
	for _, c := range w.cfg.CoinPayments.Currencies {
		buttons = append(buttons, []tg.InlineKeyboardButton{tg.NewInlineKeyboardButtonData(c, "buy_with "+c)})
	}

	keyboard := tg.NewInlineKeyboardMarkup(buttons...)
	current := w.maxModels(chatID)
	additional := w.cfg.CoinPayments.subscriptionPacketModelNumber
	overall := current + additional
	usd := w.cfg.CoinPayments.subscriptionPacketPrice
	text := fmt.Sprintf(w.tr[endpoint].SelectCurrency.Str, additional, overall, usd)
	msg := tg.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	w.sendMessage(endpoint, &messageConfig{msg})
}

func (w *worker) email(endpoint string, chatID int64) string {
	row := w.db.QueryRow("select email from emails where endpoint=? and chat_id=?", endpoint, chatID)
	var result string
	checkErr(row.Scan(&result))
	return result + "@" + w.cfg.Mail.Host
}

func (w *worker) transaction(uuid string) (status payments.StatusKind, chatID int64, endpoint string) {
	query, err := w.db.Query("select status, chat_id, endpoint from transactions where local_id=?", uuid)
	checkErr(err)
	defer func() { checkErr(query.Close()) }()
	if !query.Next() {
		return
	}
	checkErr(query.Scan(&status, &chatID, &endpoint))
	return
}

func (w *worker) buyWith(endpoint string, chatID int64, currency string) {
	found := false
	for _, c := range w.cfg.CoinPayments.Currencies {
		if currency == c {
			found = true
			break
		}
	}
	if !found {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].UnknownCurrency)
		return
	}

	w.addUser(endpoint, chatID)
	email := w.email(endpoint, chatID)
	localID := uuid.New()
	transaction, err := w.coinPaymentsAPI.CreateTransaction(w.cfg.CoinPayments.subscriptionPacketPrice, currency, email, localID.String())
	if err != nil {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].TryToBuyLater)
		lerr("create transaction failed, %v", err)
		return
	}
	kind := "coinpayments"
	timestamp := int(time.Now().Unix())
	w.mustExec(`
		insert into transactions (
			status,
			kind,
			local_id,
			chat_id,
			remote_id,
			timeout,
			amount,
			address,
			dest_tag,
			status_url,
			checkout_url,
			timestamp,
			model_number,
			currency,
			endpoint)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		payments.StatusCreated,
		kind,
		localID,
		chatID,
		transaction.TXNID,
		transaction.Timeout,
		transaction.Amount,
		transaction.Address,
		transaction.DestTag,
		transaction.StatusURL,
		transaction.CheckoutURL,
		timestamp,
		w.cfg.CoinPayments.subscriptionPacketModelNumber,
		currency,
		endpoint)

	w.sendTr(endpoint, chatID, false, w.tr[endpoint].PayThis, transaction.Amount, currency, transaction.CheckoutURL)
}

func (w *worker) cleanStatuses() {
	w.mustExec("delete from statuses where not exists(select * from signals where signals.model_id=statuses.model_id);")
}

func (w *worker) listModels(endpoint string, chatID int64) {
	statuses := w.statusesForChat(endpoint, chatID)
	var lines []string
	for _, s := range statuses {
		lines = append(lines, fmt.Sprintf(w.statusKey(endpoint, s.Status).Str, s.ModelID))
	}
	if len(lines) == 0 {
		lines = append(lines, w.tr[endpoint].NoModels.Str)
	}
	w.sendText(endpoint, chatID, false, true, w.tr[endpoint].NoModels.Parse, strings.Join(lines, "\n"))
}

func (w *worker) listOnlineModels(endpoint string, chatID int64) {
	statuses := w.statusesForChat(endpoint, chatID)
	online := 0
	for _, s := range statuses {
		if s.Status == lib.StatusOnline {
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].Online, s.ModelID)
			online++
		}
	}
	if online == 0 {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].NoOnlineModels)
	}
}

func (w *worker) feedback(endpoint string, chatID int64, text string) {
	if text == "" {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].SyntaxFeedback)
		return
	}

	w.mustExec("insert into feedback (endpoint, chat_id, text) values (?, ?, ?)", endpoint, chatID, text)
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].Feedback)
	w.sendText(endpoint, w.cfg.AdminID, true, true, parseRaw, fmt.Sprintf("Feedback from %d: %s", chatID, text))
}

func (w *worker) setLimit(chatID int64, maxModels int) {
	w.mustExec(`
		insert or replace into users (chat_id, max_models) values (?, ?)
		on conflict(chat_id) do update set max_models=excluded.max_models`,
		chatID,
		maxModels)
}

func (w *worker) unknownsNumber() int {
	var unknownsCount = 0
	for _, s := range w.unknowns {
		if s {
			unknownsCount++
		}
	}
	return unknownsCount
}

func (w *worker) userReferralsCount() int {
	query := w.db.QueryRow("select coalesce(sum(referred_users), 0) from referrals")
	return singleInt(query)
}

func (w *worker) modelReferralsCount() int {
	query := w.db.QueryRow("select coalesce(sum(referred_users), 0) from models")
	return singleInt(query)
}

func (w *worker) reports() int {
	return singleInt(w.db.QueryRow("select coalesce(sum(reports), 0) from users"))
}

func (w *worker) usersCount(endpoint string) int {
	query := w.db.QueryRow("select count(distinct chat_id) from signals where endpoint=?", endpoint)
	return singleInt(query)
}

func (w *worker) groupsCount(endpoint string) int {
	query := w.db.QueryRow("select count(distinct chat_id) from signals where endpoint=? and chat_id < 0", endpoint)
	return singleInt(query)
}

func (w *worker) activeUsersOnEndpointCount(endpoint string) int {
	query := w.db.QueryRow(
		`select count(distinct signals.chat_id) from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block = 0) and signals.endpoint=?`, endpoint)
	return singleInt(query)
}

func (w *worker) activeUsersTotalCount() int {
	query := w.db.QueryRow(
		`select count(distinct signals.chat_id) from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block = 0)`)
	return singleInt(query)
}

func (w *worker) modelsCount(endpoint string) int {
	query := w.db.QueryRow("select count(distinct model_id) from signals where endpoint=?", endpoint)
	return singleInt(query)
}

func (w *worker) modelsToQueryOnEndpointCount(endpoint string) int {
	query := w.db.QueryRow(
		`select count(distinct signals.model_id) from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block < ?) and signals.endpoint=?`,
		w.cfg.BlockThreshold,
		endpoint)
	return singleInt(query)
}

func (w *worker) modelsToQueryTotalCount() int {
	query := w.db.QueryRow(
		`select count(distinct signals.model_id) from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block < ?)`,
		w.cfg.BlockThreshold)
	return singleInt(query)
}

func (w *worker) onlineModelsCount(endpoint string) int {
	query := w.db.QueryRow(`
		select count(distinct signals.model_id) from signals
		join statuses on signals.model_id=statuses.model_id
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where statuses.status=? and (block.block is null or block.block < ?) and signals.endpoint=?`,
		lib.StatusOnline,
		w.cfg.BlockThreshold,
		endpoint)
	return singleInt(query)
}

func (w *worker) modelActiveStatus(modelID string) lib.StatusKind {
	query, err := w.db.Query(`
		select statuses.status from signals
		join statuses on signals.model_id=statuses.model_id
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where statuses.model_id=? and (block.block is null or block.block < ?)
		limit 1`,
		modelID,
		w.cfg.BlockThreshold)
	checkErr(err)
	defer func() { checkErr(query.Close()) }()
	if !query.Next() {
		return lib.StatusUnknown
	}
	var status lib.StatusKind
	checkErr(query.Scan(&status))
	return status
}

func (w *worker) heavyUsersCount(endpoint string) int {
	query := w.db.QueryRow(`
		select count(*) from (
			select 1 from signals
			left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
			where (block.block is null or block.block = 0) and signals.endpoint=?
			group by signals.chat_id
			having count(*) >= ?);`,
		endpoint,
		w.cfg.MaxModels-w.cfg.HeavyUserRemainder)
	return singleInt(query)
}

func (w *worker) transactionsOnEndpoint(endpoint string) int {
	query := w.db.QueryRow("select count(*) from transactions where endpoint=?", endpoint)
	return singleInt(query)
}

func (w *worker) transactionsOnEndpointFinished(endpoint string) int {
	query := w.db.QueryRow("select count(*) from transactions where endpoint=? and status=?", endpoint, payments.StatusFinished)
	return singleInt(query)
}

func (w *worker) statStrings(endpoint string) []string {
	stat := w.getStat(endpoint)
	return []string{
		fmt.Sprintf("Users: %d", stat.UsersCount),
		fmt.Sprintf("Groups: %d", stat.GroupsCount),
		fmt.Sprintf("Active users: %d", stat.ActiveUsersOnEndpointCount),
		fmt.Sprintf("Heavy: %d", stat.HeavyUsersCount),
		fmt.Sprintf("Models: %d", stat.ModelsCount),
		fmt.Sprintf("Models to query: %d", stat.ModelsToQueryOnEndpointCount),
		fmt.Sprintf("Queries duration: %d ms", stat.QueriesDurationMilliseconds),
		fmt.Sprintf("Error rate: %d/%d", stat.ErrorRate[0], stat.ErrorRate[1]),
		fmt.Sprintf("Memory usage: %d KiB", stat.Rss),
		fmt.Sprintf("Transactions: %d/%d", stat.TransactionsOnEndpointFinished, stat.TransactionsOnEndpointCount),
		fmt.Sprintf("Reports: %d", stat.ReportsCount),
		fmt.Sprintf("User referrals: %d", stat.UserReferralsCount),
		fmt.Sprintf("Model referrals: %d", stat.ModelReferralsCount),
	}
}

func (w *worker) stat(endpoint string) {
	w.sendText(endpoint, w.cfg.AdminID, true, true, parseRaw, strings.Join(w.statStrings(endpoint), "\n"))
}

func (w *worker) broadcast(endpoint string, text string) {
	if text == "" {
		return
	}
	if w.cfg.Debug {
		ldbg("broadcasting")
	}
	chats := w.broadcastChats(endpoint)
	for _, chatID := range chats {
		w.sendText(endpoint, chatID, true, false, parseRaw, text)
	}
	w.sendText(endpoint, w.cfg.AdminID, false, true, parseRaw, "OK")
}

func (w *worker) direct(endpoint string, arguments string) {
	parts := strings.SplitN(arguments, " ", 2)
	if len(parts) < 2 {
		w.sendText(endpoint, w.cfg.AdminID, false, true, parseRaw, "usage: /direct chatID text")
		return
	}
	whom, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		w.sendText(endpoint, w.cfg.AdminID, false, true, parseRaw, "first argument is invalid")
		return
	}
	text := parts[1]
	if text == "" {
		return
	}
	w.sendText(endpoint, whom, true, false, parseRaw, text)
	w.sendText(endpoint, w.cfg.AdminID, false, true, parseRaw, "OK")
}

func (w *worker) serveEndpoint(n string, p endpoint) {
	linf("serving endpoint %s...", n)
	var err error
	if p.CertificatePath != "" && p.CertificateKeyPath != "" {
		err = http.ListenAndServeTLS(p.ListenAddress, p.CertificatePath, p.CertificateKeyPath, nil)
	} else {
		err = http.ListenAndServe(p.ListenAddress, nil)
	}
	checkErr(err)
}

func (w *worker) serveEndpoints() {
	for n, p := range w.cfg.Endpoints {
		go w.serveEndpoint(n, p)
	}
}

func (w *worker) serveIPN() {
	go func() {
		err := http.ListenAndServe(w.cfg.CoinPayments.IPNListenAddress, w.ipnServeMux)
		checkErr(err)
	}()
}

func (w *worker) logConfig() {
	cfgString, err := json.MarshalIndent(w.cfg, "", "    ")
	checkErr(err)
	linf("config: " + string(cfgString))
}

func (w *worker) myEmail(endpoint string) {
	w.addUser(endpoint, w.cfg.AdminID)
	email := w.email(endpoint, w.cfg.AdminID)
	w.sendText(endpoint, w.cfg.AdminID, true, true, parseRaw, email)
}

func (w *worker) processAdminMessage(endpoint string, chatID int64, command, arguments string) bool {
	switch command {
	case "stat":
		w.stat(endpoint)
		return true
	case "email":
		w.myEmail(endpoint)
		return true
	case "broadcast":
		w.broadcast(endpoint, arguments)
		return true
	case "direct":
		w.direct(endpoint, arguments)
		return true
	case "set_max_models":
		parts := strings.Fields(arguments)
		if len(parts) != 2 {
			w.sendText(endpoint, chatID, false, true, parseRaw, "expecting two arguments")
			return true
		}
		who, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			w.sendText(endpoint, chatID, false, true, parseRaw, "first argument is invalid")
			return true
		}
		maxModels, err := strconv.Atoi(parts[1])
		if err != nil {
			w.sendText(endpoint, chatID, false, true, parseRaw, "second argument is invalid")
			return true
		}
		w.setLimit(who, maxModels)
		w.sendText(endpoint, chatID, false, true, parseRaw, "OK")
		return true
	}
	return false
}

func splitAddress(a string) (string, string) {
	a = strings.ToLower(a)
	parts := strings.Split(a, "@")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func (w *worker) recordForEmail(username string) *email {
	modelsQuery, err := w.db.Query(`select chat_id, endpoint from emails where email=?`, username)
	checkErr(err)
	defer func() { checkErr(modelsQuery.Close()) }()
	if modelsQuery.Next() {
		email := email{email: username}
		checkErr(modelsQuery.Scan(&email.chatID, &email.endpoint))
		return &email
	}
	return nil
}

func (w *worker) mailReceived(e *env) {
	emails := make(map[email]bool)
	for _, r := range e.rcpts {
		username, host := splitAddress(r.Email())
		if host != w.cfg.Mail.Host {
			continue
		}
		email := w.recordForEmail(username)
		if email != nil {
			emails[*email] = true
		}
	}

	for email := range emails {
		w.sendTr(email.endpoint, email.chatID, true, w.tr[email.endpoint].MailReceived,
			e.mime.GetHeader("Subject"),
			e.mime.GetHeader("From"),
			e.mime.Text)
		for _, inline := range e.mime.Inlines {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			switch {
			case strings.HasPrefix(inline.ContentType, "image/"):
				msg := tg.NewPhotoUpload(email.chatID, b)
				w.sendMessage(email.endpoint, &photoConfig{msg})
			default:
				msg := tg.NewDocumentUpload(email.chatID, b)
				w.sendMessage(email.endpoint, &documentConfig{msg})
			}
		}
		for _, inline := range e.mime.Attachments {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			msg := tg.NewDocumentUpload(email.chatID, b)
			w.sendMessage(email.endpoint, &documentConfig{msg})
		}
	}
}

func envelopeFactory(ch chan *env) func(smtpd.Connection, smtpd.MailAddress, *int) (smtpd.Envelope, error) {
	return func(c smtpd.Connection, from smtpd.MailAddress, size *int) (smtpd.Envelope, error) {
		return &env{BasicEnvelope: &smtpd.BasicEnvelope{}, from: from, ch: ch}, nil
	}
}

//noinspection SpellCheckingInspection
const letterBytes = "abcdefghijklmnopqrstuvwxyz"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func (w *worker) newRandReferralID() (id string) {
	for {
		id = randString(5)
		exists := w.db.QueryRow("select count(*) from referrals where referral_id=?", id)
		if singleInt(exists) == 0 {
			break
		}
	}
	return
}

func (w *worker) refer(followerChatID int64, referrer string) (applied appliedKind) {
	referrerChatID := w.chatForReferralID(referrer)
	if referrerChatID == nil {
		return invalidReferral
	}
	if w.userExists(followerChatID) {
		return followerExists
	}
	w.mustExec("insert into users (chat_id, max_models) values (?, ?)", followerChatID, w.cfg.MaxModels+w.cfg.FollowerBonus)
	w.mustExec(`
		insert or replace into users (chat_id, max_models) values (?, ?)
		on conflict(chat_id) do update set max_models=max_models+?`,
		*referrerChatID,
		w.cfg.MaxModels+w.cfg.ReferralBonus,
		w.cfg.ReferralBonus)
	w.mustExec("update referrals set referred_users=referred_users+1 where chat_id=?", referrerChatID)
	return referralApplied
}

func (w *worker) showReferral(endpoint string, chatID int64) {
	referralID := w.referralID(chatID)
	if referralID == nil {
		temp := w.newRandReferralID()
		referralID = &temp
		w.mustExec("insert into referrals (chat_id, referral_id) values (?, ?)", chatID, *referralID)
	}
	referralLink := fmt.Sprintf("https://t.me/%s?start=%s", w.cfg.Endpoints[endpoint].BotName, *referralID)
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].ReferralLink, referralLink, w.cfg.ReferralBonus, w.cfg.FollowerBonus)
}

func (w *worker) start(endpoint string, chatID int64, referrer string) {
	modelID := ""
	switch {
	case strings.HasPrefix(referrer, "m-"):
		modelID = referrer[2:]
		referrer = ""
	case referrer != "":
		referralID := w.referralID(chatID)
		if referralID != nil && *referralID == referrer {
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].OwnReferralLinkHit)
			return
		}
	}
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].Help)
	if chatID > 0 && referrer != "" {
		applied := w.refer(chatID, referrer)
		switch applied {
		case referralApplied:
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].ReferralApplied)
		case invalidReferral:
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidReferralLink)
		case followerExists:
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].FollowerExists)
		}
	}
	w.addUser(endpoint, chatID)
	if modelID != "" {
		if w.addModel(endpoint, chatID, modelID) {
			w.mustExec("insert or ignore into models (model_id) values (?)", modelID)
			w.mustExec("update models set referred_users=referred_users+1 where model_id=?", modelID)
		}
	}
}

func (w *worker) processIncomingCommand(endpoint string, chatID int64, command, arguments string) {
	w.resetBlock(endpoint, chatID)
	command = strings.ToLower(command)
	linf("chat: %d, command: %s %s", chatID, command, arguments)

	if chatID == w.cfg.AdminID && w.processAdminMessage(endpoint, chatID, command, arguments) {
		return
	}

	switch command {
	case "add":
		arguments = strings.Replace(arguments, "—", "--", -1)
		_ = w.addModel(endpoint, chatID, arguments)
	case "remove":
		arguments = strings.Replace(arguments, "—", "--", -1)
		w.removeModel(endpoint, chatID, arguments)
	case "list":
		w.listModels(endpoint, chatID)
	case "online":
		w.listOnlineModels(endpoint, chatID)
	case "start", "help":
		w.start(endpoint, chatID, arguments)
	case "feedback":
		w.feedback(endpoint, chatID, arguments)
	case "social":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Social)
	case "language":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Languages)
	case "version":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Version, version)
	case "remove_all":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].RemoveAll)
	case "sure_remove_all":
		w.sureRemoveAll(endpoint, chatID)
	case "want_more":
		w.wantMore(endpoint, chatID)
	case "buy":
		if w.cfg.CoinPayments == nil || w.cfg.Mail == nil {
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].UnknownCommand)
			return
		}
		w.buy(endpoint, chatID)
	case "buy_with":
		if w.cfg.CoinPayments == nil || w.cfg.Mail == nil {
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].UnknownCommand)
			return
		}
		w.buyWith(endpoint, chatID, arguments)
	case "max_models":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].YourMaxModels, w.maxModels(chatID))
	case "referral":
		w.showReferral(endpoint, chatID)
	case "":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Slash)
	default:
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].UnknownCommand)
	}
}

func (w *worker) processPeriodic(statusRequests chan []string) {
	unknownsNumber := w.unknownsNumber()
	now := time.Now()
	if w.nextErrorReport.Before(now) && unknownsNumber > w.cfg.errorThreshold {
		w.sendText(w.cfg.AdminEndpoint, w.cfg.AdminID, true, true, parseRaw, fmt.Sprintf("Dangerous error rate reached: %d/%d", unknownsNumber, w.cfg.errorDenominator))
		w.nextErrorReport = now.Add(time.Minute * time.Duration(w.cfg.ErrorReportingPeriodMinutes))
	}

	select {
	case statusRequests <- w.models():
	default:
		linf("the queue is full")
	}
}

func (w *worker) processStatusUpdate(statusUpdate lib.StatusUpdate, timestamp int) {
	if statusUpdate.Status == lib.StatusNotFound {
		if w.notFound(statusUpdate.ModelID) {
			w.reportNotFound(statusUpdate.ModelID)
			w.removeNotFound(statusUpdate.ModelID)
		}
	}
	if statusUpdate.Status != lib.StatusUnknown && w.updateStatus(statusUpdate.ModelID, statusUpdate.Status, timestamp) {
		if w.cfg.Debug {
			ldbg("reporting status of the model %s", statusUpdate.ModelID)
		}
		chats, endpoints := w.chatsForModel(statusUpdate.ModelID)
		for i, chatID := range chats {
			w.reportStatus(endpoints[i], chatID, statusUpdate.ModelID, statusUpdate.Status)
		}
	}
	w.unknowns[w.unknownsPos] = statusUpdate.Status == lib.StatusUnknown
	w.unknownsPos = (w.unknownsPos + 1) % w.cfg.errorDenominator
}

func (w *worker) processTGUpdate(p packet) {
	u := p.message
	if u.Message != nil && u.Message.Chat != nil {
		if newMembers := u.Message.NewChatMembers; newMembers != nil && len(*newMembers) > 0 {
			ourIDs := w.ourIDs()
		addedToChat:
			for _, m := range *newMembers {
				for _, ourID := range ourIDs {
					if int64(m.ID) == ourID {
						w.sendTr(p.endpoint, u.Message.Chat.ID, false, w.tr[p.endpoint].Help)
						break addedToChat
					}
				}
			}
		} else if u.Message.IsCommand() {
			w.processIncomingCommand(p.endpoint, u.Message.Chat.ID, u.Message.Command(), strings.TrimSpace(u.Message.CommandArguments()))
		} else {
			if u.Message.Text == "" {
				return
			}
			parts := strings.SplitN(u.Message.Text, " ", 2)
			for len(parts) < 2 {
				parts = append(parts, "")
			}
			w.processIncomingCommand(p.endpoint, u.Message.Chat.ID, parts[0], strings.TrimSpace(parts[1]))
		}
	}
	if u.CallbackQuery != nil {
		callback := tg.CallbackConfig{CallbackQueryID: u.CallbackQuery.ID}
		_, err := w.bots[p.endpoint].AnswerCallbackQuery(callback)
		if err != nil {
			lerr("cannot answer callback query, %v", err)
		}
		data := strings.SplitN(u.CallbackQuery.Data, " ", 2)
		chatID := int64(u.CallbackQuery.From.ID)
		if len(data) < 2 {
			data = append(data, "")
		}
		w.processIncomingCommand(p.endpoint, chatID, data[0], data[1])
	}
}

func getRss() (int64, error) {
	buf, err := ioutil.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}

	fields := strings.Split(string(buf), " ")
	if len(fields) < 2 {
		return 0, errors.New("cannot parse statm")
	}

	rss, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}

	return rss * int64(os.Getpagesize()), err
}

func (w *worker) getStat(endpoint string) statistics {
	rss, err := getRss()
	checkErr(err)
	var rusage syscall.Rusage
	checkErr(syscall.Getrusage(syscall.RUSAGE_SELF, &rusage))

	return statistics{
		UsersCount:                     w.usersCount(endpoint),
		GroupsCount:                    w.groupsCount(endpoint),
		ActiveUsersOnEndpointCount:     w.activeUsersOnEndpointCount(endpoint),
		ActiveUsersTotalCount:          w.activeUsersTotalCount(),
		HeavyUsersCount:                w.heavyUsersCount(endpoint),
		ModelsCount:                    w.modelsCount(endpoint),
		ModelsToQueryOnEndpointCount:   w.modelsToQueryOnEndpointCount(endpoint),
		ModelsToQueryTotalCount:        w.modelsToQueryTotalCount(),
		OnlineModelsCount:              w.onlineModelsCount(endpoint),
		TransactionsOnEndpointCount:    w.transactionsOnEndpoint(endpoint),
		TransactionsOnEndpointFinished: w.transactionsOnEndpointFinished(endpoint),
		QueriesDurationMilliseconds:    int(w.elapsed.Milliseconds()),
		ErrorRate:                      [2]int{w.unknownsNumber(), w.cfg.errorDenominator},
		Rss:                            rss / 1024,
		MaxRss:                         rusage.Maxrss,
		UserReferralsCount:             w.userReferralsCount(),
		ModelReferralsCount:            w.modelReferralsCount(),
		ReportsCount:                   w.reports(),
	}
}

func (w *worker) handleStat(endpoint string) func(writer http.ResponseWriter, r *http.Request) {
	return func(writer http.ResponseWriter, r *http.Request) {
		passwords, ok := r.URL.Query()["password"]
		if !ok || len(passwords[0]) < 1 {
			return
		}
		password := passwords[0]
		if password != w.cfg.StatPassword {
			return
		}
		writer.WriteHeader(http.StatusOK)
		writer.Header().Set("Content-Type", "application/json")
		statJSON, err := json.MarshalIndent(w.getStat(endpoint), "", "    ")
		checkErr(err)
		_, err = writer.Write(statJSON)
		checkErr(err)
	}
}

func (w *worker) handleIPN(writer http.ResponseWriter, r *http.Request) {
	linf("got IPN data")

	newStatus, custom, err := payments.ParseIPN(r, w.cfg.CoinPayments.IPNSecret, w.cfg.Debug)
	if err != nil {
		lerr("error on processing IPN, %v", err)
		return
	}

	switch newStatus {
	case payments.StatusFinished:
		oldStatus, chatID, endpoint := w.transaction(custom)
		if oldStatus == payments.StatusFinished {
			lerr("transaction is already finished")
			return
		}
		w.mustExec("update transactions set status=? where local_id=?", payments.StatusFinished, custom)
		w.mustExec("update users set max_models = max_models + (select coalesce(sum(model_number), 0) from transactions where local_id=?)", custom)
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].PaymentComplete, w.maxModels(chatID))
		linf("payment %s is finished", custom)
	case payments.StatusCanceled:
		w.mustExec("update transactions set status=? where local_id=?", payments.StatusCanceled, custom)
		linf("payment %s is canceled", custom)
	default:
		linf("payment %s is still pending", custom)
	}
}

func (w *worker) handleStatEndpoints() {
	for n, p := range w.cfg.Endpoints {
		http.HandleFunc(p.WebhookDomain+"/stat", w.handleStat(n))
	}
}

func (w *worker) handleIPNEndpoint() {
	w.ipnServeMux.HandleFunc(w.cfg.CoinPayments.IPNListenURL, w.handleIPN)
}

func (w *worker) incoming() chan packet {
	result := make(chan packet)
	for n, p := range w.cfg.Endpoints {
		linf("listening for a webhook for endpoint %s", n)
		incoming := w.bots[n].ListenForWebhook(p.WebhookDomain + p.ListenPath)
		go func(n string, incoming tg.UpdatesChannel) {
			for i := range incoming {
				result <- packet{message: i, endpoint: n}
			}
		}(n, incoming)
	}
	return result
}

func (w *worker) ourIDs() []int64 {
	var ids []int64
	for _, e := range w.cfg.Endpoints {
		if idx := strings.Index(e.BotToken, ":"); idx != -1 {
			id, err := strconv.ParseInt(e.BotToken[:idx], 10, 64)
			checkErr(err)
			ids = append(ids, id)
		} else {
			checkErr(errors.New("cannot get our ID"))
		}
	}
	return ids
}

func loadTLS(certFile string, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func (w *worker) referralID(chatID int64) *string {
	query, err := w.db.Query("select referral_id from referrals where chat_id=?", chatID)
	checkErr(err)
	defer func() { checkErr(query.Close()) }()
	if !query.Next() {
		return nil
	}
	var referralID string
	checkErr(query.Scan(&referralID))
	return &referralID
}

func (w *worker) chatForReferralID(referralID string) *int64 {
	query, err := w.db.Query("select chat_id from referrals where referral_id=?", referralID)
	checkErr(err)
	defer func() { checkErr(query.Close()) }()
	if !query.Next() {
		return nil
	}
	var chatID int64
	checkErr(query.Scan(&chatID))
	return &chatID
}

func main() {
	rand.Seed(time.Now().UnixNano())

	w := newWorker()
	w.logConfig()
	w.setWebhook()
	w.createDatabase()

	incoming := w.incoming()
	w.handleStatEndpoints()
	w.serveEndpoints()

	if w.cfg.CoinPayments != nil {
		w.handleIPNEndpoint()
		w.serveIPN()
	}

	mail := make(chan *env)

	if w.cfg.Mail != nil {
		smtp := &smtpd.Server{
			Hostname:  w.cfg.Mail.Host,
			Addr:      w.cfg.Mail.ListenAddress,
			OnNewMail: envelopeFactory(mail),
			TLSConfig: w.mailTLS,
		}
		go func() {
			err := smtp.ListenAndServe()
			checkErr(err)
		}()
	}

	var periodicTimer = time.NewTicker(time.Duration(w.cfg.PeriodSeconds) * time.Second)
	statusRequests, statusUpdates, elapsed := w.startChecker(w.cfg.UsersOnlineEndpoint, w.clients, w.cfg.Headers, w.cfg.IntervalMs, w.cfg.Debug)
	statusRequests <- w.models()
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	for {
		select {
		case e := <-elapsed:
			w.elapsed = e
		case <-periodicTimer.C:
			runtime.GC()
			w.processPeriodic(statusRequests)
		case statusUpdate := <-statusUpdates:
			timestamp := int(time.Now().Unix())
			w.processStatusUpdate(statusUpdate, timestamp)
		case u := <-incoming:
			w.processTGUpdate(u)
		case m := <-mail:
			w.mailReceived(m)
		case s := <-signals:
			linf("got signal %v", s)
			w.removeWebhook()
			return
		}
	}
}
