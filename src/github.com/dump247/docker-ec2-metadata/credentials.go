package main

import (
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

const (
	maxSessionNameLen int = 32
)

var (
	// matches char that is not valid in a STS role session name
	invalidSessionNameRegexp *regexp.Regexp = regexp.MustCompile(`[^\w+=,.@-]`)
)

type Credentials struct {
	AccessKey   string
	Expiration  time.Time
	GeneratedAt time.Time
	RoleArn     RoleArn
	SecretKey   string
	Token       string
}

func (self Credentials) ExpiredNow() bool {
	return self.ExpiredAt(time.Now())
}

func (self Credentials) ExpiredAt(at time.Time) bool {
	return at.After(self.Expiration)
}

func (self Credentials) ExpiresIn(d time.Duration) bool {
	return self.ExpiredAt(time.Now().Add(-d))
}

type ContainerCredentials struct {
	ContainerInfo
	Credentials
}

func (self ContainerCredentials) IsValid(container ContainerInfo) bool {
	return self.ContainerInfo.IamRole.Equals(container.IamRole) &&
		self.ContainerInfo.Id == container.Id &&
		!self.Credentials.ExpiresIn(5*time.Minute)
}

type CredentialsProvider struct {
	container            ContainerService
	awsSts               *sts.STS
	defaultIamRoleArn    RoleArn
	defaultIamPolicy     string
	containerCredentials map[string]ContainerCredentials
	lock                 sync.Mutex
}

func NewCredentialsProvider(awsSession *session.Session, container ContainerService, defaultIamRoleArn RoleArn, defaultIamPolicy string) *CredentialsProvider {
	return &CredentialsProvider{
		container:            container,
		awsSts:               sts.New(awsSession),
		defaultIamRoleArn:    defaultIamRoleArn,
		defaultIamPolicy:     defaultIamPolicy,
		containerCredentials: make(map[string]ContainerCredentials),
	}
}

func (self *CredentialsProvider) CredentialsForIP(containerIP string) (Credentials, error) {
	self.lock.Lock()
	defer self.lock.Unlock()

	container, err := self.container.ContainerForIP(containerIP)

	if err != nil {
		return Credentials{}, err
	}

	oldCredentials, found := self.containerCredentials[containerIP]

	if !found || !oldCredentials.IsValid(container) {
		roleArn := container.IamRole
		iamPolicy := container.IamPolicy

		if roleArn.Empty() {
			roleArn = self.defaultIamRoleArn

			if len(iamPolicy) == 0 {
				iamPolicy = self.defaultIamPolicy
			}
		}

		role, err := self.AssumeRole(roleArn, iamPolicy, generateSessionName(self.container.TypeName(), container.Id))

		if err != nil {
			return Credentials{}, err
		}

		oldCredentials = ContainerCredentials{container, role}
		self.containerCredentials[containerIP] = oldCredentials
	}

	return oldCredentials.Credentials, nil
}

func (self *CredentialsProvider) AssumeRole(roleArn RoleArn, iamPolicy, sessionName string) (Credentials, error) {
	var policy *string = nil

	if len(iamPolicy) > 0 {
		policy = aws.String(iamPolicy)
	}

	resp, err := self.awsSts.AssumeRole(&sts.AssumeRoleInput{
		DurationSeconds: aws.Int64(3600), // Max is 1 hour
		Policy:          policy,
		RoleArn:         aws.String(roleArn.String()),
		RoleSessionName: aws.String(sessionName),
	})

	if err != nil {
		return Credentials{}, err
	}

	return Credentials{
		AccessKey:   *resp.Credentials.AccessKeyId,
		SecretKey:   *resp.Credentials.SecretAccessKey,
		Token:       *resp.Credentials.SessionToken,
		Expiration:  *resp.Credentials.Expiration,
		GeneratedAt: time.Now(),
	}, nil
}

func generateSessionName(platform, containerId string) string {
	sessionName := fmt.Sprintf("%s-%s", platform, containerId)
	return invalidSessionNameRegexp.ReplaceAllString(sessionName, "_")[0:maxSessionNameLen]
}
