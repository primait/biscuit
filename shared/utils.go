package shared

import (
	"log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"gopkg.in/yaml.v2"
)

// MustYaml serializes i to a YAML string, or panics if it fails to do so.
func MustYaml(i interface{}) string {
	bytes, err := yaml.Marshal(i)
	if err != nil {
		panic(err)
	}
	return string(bytes)
}

func GetNewSession() *session.Session {
	session, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable, // Must be set to enable
	})
	if err != nil {
		log.Fatal("error:", err)
	}
	return session
}

func GetNewSessionWithRegion(region string) *session.Session {
	session, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable, // Must be set to enable
		Config:            *aws.NewConfig().WithRegion(region),
	})
	if err != nil {
		log.Fatal("error:", err)
	}
	return session
}
