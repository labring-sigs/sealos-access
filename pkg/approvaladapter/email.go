/*
Copyright 2026 labring.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package approvaladapter

import (
	"context"
	"errors"
	"fmt"
	"net/smtp"
	"strings"
	"time"
)

// MailSender is injectable for tests. Production code uses smtp.SendMail.
type MailSender func(addr string, auth smtp.Auth, from string, to []string, message []byte) error

// EmailNotifier sends only the one-time Broker reference and non-secret
// approval metadata. It never sends a Kubernetes token or kubeconfig.
type EmailNotifier struct {
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	From         string
	Subject      string
	Send         MailSender
}

func (n *EmailNotifier) Notify(ctx context.Context, notification Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.SMTPHost == "" || n.SMTPPort <= 0 || !validEmail(n.From) || !validEmail(notification.Recipient) || !validReference(notification.ApprovalReference) {
		return errors.New("email notifier configuration or notification is invalid")
	}
	if strings.ContainsAny(n.Subject, "\r\n") {
		return errors.New("email subject is invalid")
	}
	subject := n.Subject
	if subject == "" {
		subject = "Internal Kubernetes credential approval"
	}
	sender := n.Send
	if sender == nil {
		sender = smtp.SendMail
	}
	var auth smtp.Auth
	if n.SMTPUsername != "" {
		auth = smtp.PlainAuth("", n.SMTPUsername, n.SMTPPassword, n.SMTPHost)
	}
	message := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nAn elevated internal Kubernetes credential was approved.\r\n\r\nCluster ID: %s\r\nApproval ID: %s\r\nProfile: %s\r\nApproval reference: %s\r\nReference expires: %s\r\n\r\nRedeem this reference with the internal-user CLI as the approved OIDC user.\r\n", n.From, notification.Recipient, subject, notification.ClusterID, notification.ApprovalID, notification.Profile, notification.ApprovalReference, notification.ExpiresAt.UTC().Format(time.RFC3339)))
	return sender(fmt.Sprintf("%s:%d", n.SMTPHost, n.SMTPPort), auth, n.From, []string{notification.Recipient}, message)
}
