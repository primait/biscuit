package awskms

import (
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
)

type cloudformationStack struct {
	region,
	stackName string
	params       map[string]string
	templateBody *string
	templateURL  *string
}

func (s *cloudformationStack) parameterList() (output []*cloudformation.Parameter) {
	for key, value := range s.params {
		output = append(output,
			&cloudformation.Parameter{
				ParameterKey:   aws.String(key),
				ParameterValue: aws.String(value),
			})
	}
	return
}

func (s *cloudformationStack) createAndWait() (map[string]string, error) {
	session, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable, // Must be set to enable
		Config:            *aws.NewConfig().WithRegion(s.region),
	})
	if err != nil {
		log.Fatal("error:", err)
	}
	cfclient := cloudformation.New(session)
	createStackInput := &cloudformation.CreateStackInput{
		StackName:    &s.stackName,
		Capabilities: []*string{aws.String("CAPABILITY_IAM")},
		OnFailure:    aws.String("ROLLBACK"),
		Parameters:   s.parameterList(),
		TemplateBody: s.templateBody,
		TemplateURL:  s.templateURL,
	}
	createStackOutput, err := cfclient.CreateStack(createStackInput)
	if err != nil {
		return nil, err
	}
	fmt.Printf("%s: Waiting for CloudFormation stack %s.\n", s.region, *createStackOutput.StackId)
	describeStackInput := &cloudformation.DescribeStacksInput{StackName: createStackOutput.StackId}
	if err := cfclient.WaitUntilStackCreateComplete(describeStackInput); err != nil {
		return nil, err
	}
	describeStackOutput, err := cfclient.DescribeStacks(&cloudformation.DescribeStacksInput{
		StackName: createStackOutput.StackId})
	if err != nil {
		return nil, err
	}
	if len(describeStackOutput.Stacks) == 0 {
		return nil, fmt.Errorf("DescribeStacks returned an empty stack list for %s.", *createStackOutput.StackId)
	}

	outputs := make(map[string]string)
	for _, output := range describeStackOutput.Stacks[0].Outputs {
		outputs[*output.OutputKey] = *output.OutputValue
	}
	return outputs, nil
}
