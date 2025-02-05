package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"github.com/artpar/api2go"
	"github.com/artpar/go-guerrilla/authenticators"
	"github.com/artpar/go-guerrilla/backends"
	"github.com/artpar/go-guerrilla/mail"
	"github.com/artpar/go-guerrilla/response"
	"github.com/artpar/parsemail"
	"github.com/bjarneh/latinx"
	"github.com/daptin/daptin/server/auth"
	"github.com/daptin/daptin/server/resource"
	"net/http"
	log "github.com/sirupsen/logrus"
	"strings"
)

// ----------------------------------------------------------------------------------
// Processor Name: sql
// ----------------------------------------------------------------------------------
// Description   : Saves the e.Data (email data) and e.DeliveryHeader together in sql
//               : using the hash generated by the "hash" processor and stored in
//               : e.Hashes
// ----------------------------------------------------------------------------------
// Config Options: mail_table string - name of table for storing emails
//               : sql_driver string - database driver name, eg. mysql
//               : sql_dsn string - driver-specific data source name
//               : primary_mail_host string - primary host name
// --------------:-------------------------------------------------------------------
// Input         : e.Data
//               : e.DeliveryHeader generated by ParseHeader() processor
//               : e.MailFrom
//               : e.Subject - generated by by ParseHeader() processor
// ----------------------------------------------------------------------------------
// Output        : Sets e.QueuedId with the first item fromHashes[0]
// ----------------------------------------------------------------------------------

type stmtCache [backends.GuerrillaDBAndRedisBatchMax]*sql.Stmt

type SQLProcessorConfig struct {
	PrimaryHost string `json:"primary_mail_host"`
	DbResource  *resource.DbResource
}

type SQLProcessor struct {
	cache  stmtCache
	config *SQLProcessorConfig
}

func (s *SQLProcessor) fillAddressFromHeader(e *mail.Envelope, headerKey string) string {
	if v, ok := e.Header[headerKey]; ok {
		addr, err := mail.NewAddress(v[0])
		if err != nil {
			return ""
		}
		return addr.String()
	}
	return ""
}

// compressedData struct will be compressed using zlib when printed via fmt
type Compressor interface {
	String() string
}

func trimToLimit(str string, limit int) string {
	ret := strings.TrimSpace(str)
	if len(str) > limit {
		ret = str[:limit]
	}
	return ret
}

type DaptinSmtpAuthenticator struct {
	dbResource *resource.DbResource
	config     backends.BackendConfig
}

func (dsa *DaptinSmtpAuthenticator) VerifyLOGIN(login, passwordBase64 string) bool {

	username, err := base64.StdEncoding.DecodeString(login)
	if err != nil {
		return false
	}
	mailAccount, err := dsa.dbResource.GetUserMailAccountRowByEmail(string(username))
	if err != nil {
		return false
	}
	password, err := base64.StdEncoding.DecodeString(passwordBase64)
	if err != nil {
		return false
	}

	if resource.BcryptCheckStringHash(string(password), mailAccount["password"].(string)) {
		return true
	}

	return false
}

//VerifyPLAIN(login, password string) bool
//VerifyGSSAPI(login, password string) bool
//VerifyDIGESTMD5(login, password string) bool
//VerifyMD5(login, password string) bool
func (dsa *DaptinSmtpAuthenticator) VerifyCRAMMD5(challenge, authString string) bool {
	return false
}
func (dsa *DaptinSmtpAuthenticator) GenerateCRAMMD5Challenge() (string, error) {
	return "", nil
}
func (dsa *DaptinSmtpAuthenticator) ExtractLoginFromAuthString(authString string) string {
	return ""
}
func (dsa *DaptinSmtpAuthenticator) DecodeLogin(login string) (string, error) {
	username, err := base64.StdEncoding.DecodeString(login)
	return string(username), err
}

func (dsa *DaptinSmtpAuthenticator) GetAdvertiseAuthentication(authType []string) string {
	return "250-AUTH " + strings.Join(authType, " ") + "\r\n"
}

func (dsa *DaptinSmtpAuthenticator) GetMailSize(login string, defaultSize int64) int64 {
	return 10000
}

func DaptinSmtpAuthenticatorCreator(dbResource *resource.DbResource) func(config backends.BackendConfig) authenticators.Authenticator {
	return func(config backends.BackendConfig) authenticators.Authenticator {
		return &DaptinSmtpAuthenticator{
			dbResource: dbResource,
			config:     config,
		}
	}
}

func DaptinSmtpDbResource(dbResource *resource.DbResource) func() backends.Decorator {

	return func() backends.Decorator {
		var config *SQLProcessorConfig
		//var db *sql.DB
		s := &SQLProcessor{}

		// open the database connection (it will also check if we can select the table)
		backends.Svc.AddInitializer(backends.InitializeWith(func(backendConfig backends.BackendConfig) error {
			configType := backends.BaseConfig(&SQLProcessorConfig{})
			bcfg, err := backends.Svc.ExtractConfig(backendConfig, configType)
			if err != nil {
				return err
			}
			config = bcfg.(*SQLProcessorConfig)
			s.config = config
			return nil
		}))

		// shutdown will close the database connection
		backends.Svc.AddShutdowner(backends.ShutdownWith(func() error {
			//if db != nil {
			//	return db.Close()
			//}
			return nil
		}))

		return func(p backends.Processor) backends.Processor {
			return backends.ProcessWith(func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error) {

				if task == backends.TaskSaveMail {
					var to, body string

					hash := ""
					if len(e.Hashes) > 0 {
						hash = e.Hashes[0]
						e.QueuedId = e.Hashes[0]
					}

					var co Compressor
					// a compressor was set by the Compress processor
					if c, ok := e.Values["zlib-compressor"]; ok {
						co = c.(Compressor)
					}

					for i := range e.RcptTo {
						// use the To header, otherwise rcpt to
						to = trimToLimit(s.fillAddressFromHeader(e, "To"), 255)
						if to == "" {
							// trimToLimit(strings.TrimSpace(e.RcptTo[i].User)+"@"+config.PrimaryHost, 255)
							to = trimToLimit(strings.TrimSpace(e.RcptTo[i].String()), 255)
						}
						mid := trimToLimit(s.fillAddressFromHeader(e, "Message-Id"), 255)
						if mid == "" {
							mid = fmt.Sprintf("%s.%s@%s", hash, e.RcptTo[i].User, config.PrimaryHost)
						}
						// replyTo is the 'Reply-to' header, it may be blank
						replyTo := trimToLimit(s.fillAddressFromHeader(e, "Reply-To"), 255)
						// sender is the 'Sender' header, it may be blank
						sender := trimToLimit(s.fillAddressFromHeader(e, "Sender"), 255)

						recipient := trimToLimit(strings.TrimSpace(e.RcptTo[i].String()), 255)
						contentType := ""
						if v, ok := e.Header["Content-Type"]; ok {
							contentType = trimToLimit(v[0], 255)
						}

						mailBytes := e.Data.Bytes()
						parsedMail, err := parsemail.Parse(bytes.NewReader(mailBytes))
						body = parsedMail.TextBody

						if strings.Index(parsedMail.Header.Get("Content-type"), "iso-8859-1") > -1 {
							converter := latinx.Get(latinx.ISO_8859_1)
							textBodyBytes, err := converter.Decode([]byte(body))
							if err != nil {
								log.Printf("Failed to convert iso 8859 to utf8: %v", err)
							}
							body = string(textBodyBytes)
						}

						if err != nil {
							log.Printf("Failed to parse email body: %v", err)
						}

						log.Printf("Authorized login: %v", e.AuthorizedLogin)

						var mailBody interface{}
						var mailSize int
						// `mail` column
						if body == "redis" {
							// data already saved in redis
							mailBody = ""
						} else if co != nil {
							// use a compressor (automatically adds e.DeliveryHeader)
							//mailBytes = []byte(co.String())
						}
						mailSize = len(mailBytes)
						mailBody = base64.StdEncoding.EncodeToString(mailBytes)
						pr := &http.Request{}

						//mail_server, err := dbResource.GetObjectByWhereClause("mail_server", "hostname", e.RcptTo[i].Host)

						mailAccount, err := dbResource.GetUserMailAccountRowByEmail(e.RcptTo[i].String())

						if err != nil {
							continue
						}

						user, _, err := dbResource.GetSingleRowByReferenceId("user_account", mailAccount["user_account_id"].(string))

						sessionUser := &auth.SessionUser{
							UserId:          user["id"].(int64),
							UserReferenceId: user["reference_id"].(string),
							Groups:          dbResource.GetObjectUserGroupsByWhere("user_account", "id", user["id"].(int64)),
						}

						mailBox, err := dbResource.GetMailAccountBox(mailAccount["id"].(int64), "INBOX")

						if err != nil {
							mailBox, err = dbResource.CreateMailAccountBox(
								mailAccount["reference_id"].(string),
								sessionUser,
								"INBOX")
							if err != nil {
								continue
							}
						}

						//if err == nil {
						//
						//	sessionUser = &auth.SessionUser{
						//		UserId:          user["id"].(int64),
						//		UserReferenceId: user["reference_id"].(string),
						//		Groups:          []auth.GroupPermission{},
						//	}
						//}

						pr = pr.WithContext(context.WithValue(context.Background(), "user", sessionUser))

						req := &api2go.Request{
							PlainRequest: pr,
						}

						model := api2go.Api2GoModel{
							Data: map[string]interface{}{
								"message_id":     mid,
								"mail_id":        hash,
								"from_address":   trimToLimit(e.MailFrom.String(), 255),
								"to_address":     to,
								"sender_address": sender,
								"subject":        trimToLimit(e.Subject, 255),
								"body":           body,
								"mail":           mailBody,
								"spam_score":     0,
								"hash":           hash,
								//"uid":              nextUid,
								"content_type":     contentType,
								"reply_to_address": replyTo,
								"internal_date":    parsedMail.Date,
								"recipient":        recipient,
								"has_attachment":   len(parsedMail.Attachments) > 0,
								"ip_addr":          e.RemoteIP,
								"return_path":      trimToLimit(e.MailFrom.String(), 255),
								"is_tls":           e.TLS,
								"mail_box_id":      mailBox["reference_id"],
								"user_account_id":  mailAccount["user_account_id"],
								"seen":             false,
								"recent":           true,
								"flags":            "RECENT",
								"size":             mailSize,
							},
						}
						_, err = dbResource.Cruds["mail"].CreateWithoutFilter(&model, *req)
						resource.CheckErr(err, "Failed to store mail")
						//err1 := dbResource.Cruds["mail"].IncrementMailBoxUid(mailBox["id"].(int64), nextUid+1)
						//resource.CheckErr(err1, "Failed to increment uid for mailbox")

						if err != nil {
							return backends.NewResult(fmt.Sprint("554 Error: could not save email")), backends.StorageError
						}
					}

					// continue to the next Processor in the decorator chain
					return p.Process(e, task)
				} else if task == backends.TaskValidateRcpt {
					// if you need to validate the e.Rcpt then change to:¬
					if len(e.RcptTo) > 0 {
						// since this is called each time a recipient is added
						// validate only the _last_ recipient that was appended
						last := e.RcptTo[len(e.RcptTo)-1]
						if len(last.User) > 255 {
							// return with an error
							return backends.NewResult(response.Canned.FailRcptCmd), backends.NoSuchUser
						}
					}
					// continue to the next processor
					return p.Process(e, task)
				} else {
					return p.Process(e, task)
				}
			})
		}
	}

}
