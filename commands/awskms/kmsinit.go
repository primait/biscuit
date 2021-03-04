package awskms

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/primait/biscuit/keymanager"
	"github.com/primait/biscuit/shared"
	"github.com/primait/biscuit/store"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	arnDetailsMessage = "Users may be referenced by their naked username (ex: 'jeff') or prefixed with user/ (ex:" +
		" 'user/jeff'). Roles may be prefixed with role/ (ex: 'role/webserver'). When the naked or " +
		"prefixed forms are used, the full ARN is composed by using the account ID of the user " +
		"invoking the command. Principals prefixed with arn: are passed to AWS verbatim."
)

type kmsInit struct {
	regions           *[]string
	label             *string
	createMissingKeys *bool
	createSimpleRoles *bool
	disableIam        *bool
	administratorArns,
	userArns,
	filename,
	algorithm,
	cloudformationTemplateURL *string
	keyCloudformationTemplate string
}

// NewKmsInit configures the command to configure AWS.
func NewKmsInit(c *kingpin.CmdClause, keyCloudformationTemplate string) shared.Command {
	params := &kmsInit{keyCloudformationTemplate: keyCloudformationTemplate}
	params.regions = regionsFlag(c)
	params.label = labelFlag(c)
	params.createMissingKeys = c.Flag("create-missing-keys",
		"Provision regions that are not already configured for the speccified label.").Bool()
	params.createSimpleRoles = c.Flag("create-simple-roles",
		"Create simplified roles that are a allowed full encrypt or decrypt privileges under the created keys"+
			". Note that this requires sufficient IAM privileges to call iam:CreateRole.").Bool()
	params.administratorArns = c.Flag("administrators",
		"Comma-delimited list of IAM users, IAM roles, and AWS services ARNs that will "+
			"have administration privileges in the key policy attached to the new keys. "+
			arnDetailsMessage).
		Short('d').
		PlaceHolder("ARN").
		String()
	params.userArns = c.Flag("users",
		"Comma-delimited list of IAM users, IAM roles, and AWS services ARNs that will have "+
			"user privileges in the key policy attached to the new keys. "+arnDetailsMessage).
		Short('u').
		PlaceHolder("ARN").
		String()
	params.disableIam = c.Flag("disable-iam-policies",
		"Create KMS keys that will not evaluate IAM policies. If disabled, only the Key Policy document will "+
			"be evaluated when KMS authorizes API calls. Note that using this setting will prevent the "+
			"root account from accessing this key, and can require contacting AWS support for resolving "+
			"configuration problems.").
		Bool()
	params.cloudformationTemplateURL = c.Flag("cloudformation-template-url",
		"Full URL to the CloudFormation template to use. This overrides the built-in template.").
		PlaceHolder("URL").
		String()
	params.filename = shared.FilenameFlag(c)
	params.algorithm = shared.AlgorithmFlag(c)
	return params
}

// Run runs the command.
func (w *kmsInit) Run() error {
	regionKeys, err := w.discoverOrCreateKeys()
	if err != nil {
		return err
	}

	database := store.NewFileStore(*w.filename)

	// If the file exists, we'll make changes to its template rather than replace it.
	keyConfigs, err := database.Get(store.KeyTemplateName)
	if err != nil && !(err == store.ErrNameNotFound || store.IsProbablyNewStore(err)) {
		return err
	}

	// Convert keyConfigs into a map of KeyID -> Value so that we can replace any existing
	// entries for these keys. This allows the algorithm parameter to change w/o creating
	// duplicate entries, and leaves other entries alone.
	keyIDToValue := make(map[string]store.Value)
	for _, value := range keyConfigs {
		keyIDToValue[keymanager.KmsLabel+value.KeyID] = value
	}

	// Iterate over the discovered/created keys and set values for them in keyIDToValue.
	for _, keyArn := range regionKeys {
		keyIDToValue[keymanager.KmsLabel+keyArn] = store.Value{
			Key: store.Key{
				KeyID:      keyArn,
				KeyManager: keymanager.KmsLabel,
				Algorithm:  *w.algorithm,
			},
		}
	}

	// Turn keyIDToValue back into an array by converting the map values into a list.
	var updatedTemplate []store.Value
	for _, v := range keyIDToValue {
		updatedTemplate = append(updatedTemplate, v)
	}

	fmt.Printf("The template used by %s has been updated to include %s: %s.\n",
		*w.filename,
		pluralize("key", len(regionKeys)),
		stringStringMapValues(regionKeys))

	return database.Put(store.KeyTemplateName, updatedTemplate)
}

func collectRegionInfo(stackName, keyAlias string, regions []string) (map[string]string, []string, error) {
	regionErrors := make(map[string][]error)
	regionKeys := make(map[string]string)
	var regionsMissing []string

	for _, region := range regions {
		var keyExists, stackExists bool

		if exists, err := checkCloudFormationStackExists(stackName, region); err != nil {
			regionErrors[region] = append(regionErrors[region], err)
		} else {
			stackExists = exists
		}

		if regionKey, err := checkKmsKeyExists(keyAlias, region); err != nil {
			regionErrors[region] = append(regionErrors[region], err)
		} else if len(regionKey) > 0 {
			keyExists = true
			regionKeys[region] = regionKey
		} else {
			regionsMissing = append(regionsMissing, region)
		}

		if !keyExists && stackExists {
			regionErrors[region] = append(regionErrors[region],
				fmt.Errorf("A CloudFormation stack named '%s' exists, but the corresponding "+
					"key alias '%s' does not. The most likely cause of this is that a key "+
					"was incompletely deleted. You can resolve this by deleting the stack "+
					"or by using an alternate label. To delete the stack, run: aws --region %s "+
					"cloudformation delete-stack --stack-name %s. ", stackName, keyAlias, region,
					stackName))
		}
	}

	var err error
	for region, errorList := range regionErrors {
		for _, oneErr := range errorList {
			fmt.Fprintf(os.Stderr, "%s: %s\n", region, oneErr)
		}
		err = fmt.Errorf("Please manually resolve the issues and try again.")
	}

	return regionKeys, regionsMissing, err
}

func checkCloudFormationStackExists(stackName, region string) (bool, error) {
	cfclient := cloudformation.New(shared.GetNewSessionWithRegion(region))
	_, err := cfclient.DescribeStacks(&cloudformation.DescribeStacksInput{
		StackName: aws.String(stackName),
	})
	if err == nil {
		return true, nil
	}
	if awsErr, ok := err.(awserr.Error); ok {
		if awsErr.Code() == "ValidationError" &&
			strings.Contains(awsErr.Message(), "does not exist") {
			return false, nil
		}
	}
	return false, fmt.Errorf("%s", err)
}

func checkKmsKeyExists(keyAlias, region string) (string, error) {
	var foundAliasArn string
	kmsClient := kms.New(shared.GetNewSessionWithRegion(region))
	var callbackErr error
	fp := func(p *kms.ListAliasesOutput, lastPage bool) bool {
		for _, aliasRecord := range p.Aliases {
			if *aliasRecord.AliasName != keyAlias {
				continue
			}
			keyDetails, err := kmsClient.DescribeKey(&kms.DescribeKeyInput{KeyId: aliasRecord.TargetKeyId})
			if err != nil {
				fmt.Fprintf(os.Stderr, "DescribeKey failed: %s", err)
				callbackErr = err
				return false
			}
			if !*keyDetails.KeyMetadata.Enabled {
				callbackErr = fmt.Errorf(
					"There is a KMS key in %s with a matching alias, but the key is "+
						"disabled. If the alias is "+
						"no longer in use, you may try again after deleting the alias"+
						". To delete the alias, run: "+
						"aws --region "+
						"%s kms delete-alias --alias-name %s\n", region, region, keyAlias)
				return false
			}
			foundAliasArn = *aliasRecord.AliasArn
		}
		return true
	}
	if err := kmsClient.ListAliasesPages(nil, fp); err != nil {
		return foundAliasArn, err
	}
	return foundAliasArn, callbackErr
}

func (w *kmsInit) discoverOrCreateKeys() (map[string]string, error) {
	fmt.Printf("Checking %s for the '%s' label.\n",
		friendlyJoin(*w.regions),
		*w.label)

	aliasName := kmsAliasName(*w.label)
	stackName := cfStackName(*w.label)

	existingAliases, regionsMissingKeys, err := collectRegionInfo(stackName, aliasName, *w.regions)
	if err != nil {
		return nil, err
	}
	if len(existingAliases) > 0 && len(regionsMissingKeys) > 0 && !*w.createMissingKeys {
		return nil, fmt.Errorf("You've requested to use %d regions, but %d regions already "+
			"have keys provisioned for "+
			"label '%s'. If you'd like the additional regions to be provisioned, re-run "+
			"this command with the --create-missing-keys flag. If you'd like to use a new set of keys, "+
			"re-run with the --label flag. If you'd like to choose a different set of regions, use"+
			"the --regions flag. Run 'biscuit kms init --help' for more information.",
			len(*w.regions),
			len(existingAliases),
			*w.label)
	}
	if len(existingAliases) > 0 {
		fmt.Printf("Found %d pre-existing keys.\n", len(existingAliases))
	}
	if len(existingAliases) == 0 || *w.createMissingKeys {
		finalAdminArns, finalUserArns, err := w.constructArns()
		if err != nil {
			return nil, err
		}

		fmt.Printf("%s %s need to be provisioned.\n", pluralize("Region", len(regionsMissingKeys)),
			friendlyJoin(regionsMissingKeys))

		errs := make(chan error, len(regionsMissingKeys))
		var wg sync.WaitGroup
		for _, region := range regionsMissingKeys {
			wg.Add(1)
			go func(region string) {
				defer wg.Done()
				started := time.Now()
				fmt.Printf("%s: Creating resources using CloudFormation. This may take a while.\n", region)
				existingAliases[region], err = w.createKeyInRegion(region, stackName,
					aliasName, finalAdminArns, finalUserArns)
				if err != nil {
					errs <- fmt.Errorf("%s: %s", region, err)
				}
				fmt.Fprintf(os.Stderr, "%s: finished in %s.\n", region, time.Since(started))
			}(region)
		}
		wg.Wait()
		close(errs)
		for err = range errs {
			fmt.Fprintf(os.Stderr, "%s\n", err)
		}
		if err != nil {
			return nil, err
		}
	}
	return existingAliases, nil
}

// createKeyInRegion creates a key for a region and returns the Alias's ARN.
func (w *kmsInit) createKeyInRegion(region, stackName, aliasName string, finalAdminArns, finalUserArns []string) (string, error) {
	specs := cloudformationStack{
		params: map[string]string{
			"AdministratorPrincipals":            strings.Join(finalAdminArns, ","),
			"UserPrincipals":                     strings.Join(finalUserArns, ","),
			"KeyDescription":                     "Key used for securing secrets (" + *w.label + ").",
			"CreateSimpleRoles":                  truefalse(*w.createSimpleRoles),
			"AllowIAMPoliciesToControlKeyAccess": truefalse(!*w.disableIam),
		},
		region:    region,
		stackName: stackName,
	}
	if len(*w.cloudformationTemplateURL) > 0 {
		specs.templateURL = w.cloudformationTemplateURL
	} else {
		specs.templateBody = &w.keyCloudformationTemplate
	}
	outputs, err := specs.createAndWait()
	if err != nil {
		return "", err
	}
	keyArn := outputs["KeyArn"]
	if keyArn == "" {
		return "", fmt.Errorf("Stack %s does not have an Output named KeyArn.", stackName)
	}

	aliasARN, err := createAlias(region, aliasName, keyArn)
	return aliasARN, err
}

func createAlias(region, aliasName, keyArn string) (string, error) {
	fmt.Printf("%s: creating alias '%s' for key %s.\n", region, aliasName, keyArn)
	client := kmsHelper{kms.New(shared.GetNewSessionWithRegion(region))}
	if _, err := client.CreateAlias(&kms.CreateAliasInput{
		TargetKeyId: aws.String(keyArn),
		AliasName:   aws.String(aliasName)}); err != nil {
		return "", err
	}
	fmt.Printf("%s: fetching ARN for the new alias.\n", region)
	aliasListEntry, err := client.GetAliasByName(aliasName)
	if err != nil {
		return "", err
	}
	if aliasListEntry == nil {
		return "", errors.New("failed to discover ARN of new alias")
	}
	return *aliasListEntry.AliasArn, nil
}

func truefalse(iff bool) string {
	if iff {
		return "true"
	}
	return "false"
}

func (w *kmsInit) constructArns() ([]string, []string, error) {
	stsClient := sts.New(shared.GetNewSession())
	callerIdentity, err := stsClient.GetCallerIdentity(nil)
	if err != nil {
		return nil, nil, err
	}
	awsAccountID := *callerIdentity.Account
	fmt.Printf("Detected account ID #%s and that I am %s.\n", awsAccountID, *callerIdentity.Arn)
	adminArns := cleanArnList(awsAccountID, *w.administratorArns+","+*callerIdentity.Arn)
	if err := validateArnList(adminArns); err != nil {
		return nil, nil, fmt.Errorf("Administrator ARNs: %s", err)
	}
	userArns := cleanArnList(awsAccountID, *w.userArns+","+*callerIdentity.Arn)
	if err := validateArnList(userArns); err != nil {
		return nil, nil, fmt.Errorf("User ARNs: %s", err)
	}
	fmt.Printf("Administrative actions will be allowed by %s\n", adminArns)
	fmt.Printf("User actions will be allowed by %s\n", userArns)
	return adminArns, userArns, nil
}

func cleanArnList(accountID, arns string) []string {
	cleaned := make(map[string]struct{})
	for _, arn := range strings.Split(arns, ",") {
		arn := cleanArn(accountID, arn)
		if len(arn) > 0 {
			cleaned[arn] = struct{}{}
		}
	}
	return stringsetToList(cleaned)
}

func cleanArn(accountID, arn string) string {
	arn = strings.TrimSpace(arn)
	if len(arn) == 0 {
		return ""
	}
	if strings.HasPrefix(arn, "arn:") {
		return arn
	} else if !(strings.HasPrefix(arn, "user/") || strings.HasPrefix(arn, "role/")) {
		return fmt.Sprintf("arn:aws:iam::%s:user/%s", accountID, arn)
	} else {
		return fmt.Sprintf("arn:aws:iam::%s:%s", accountID, arn)
	}
}

func stringsetToList(input map[string]struct{}) []string {
	results := []string{}
	for key := range input {
		results = append(results, key)
	}
	sort.Strings(results)
	return results
}

func validateArnList(arns []string) error {
	if len(arns) == 0 {
		return errors.New("There must be at least one entry.")
	}
	return nil
}

func stringStringMapValues(input map[string]string) []string {
	results := []string{}
	for _, value := range input {
		results = append(results, value)
	}
	sort.Strings(results)
	return results
}

func pluralize(word string, count int) string {
	if count > 1 {
		return word + "s"
	}
	return word
}

func friendlyJoin(words []string) string {
	if len(words) == 0 {
		return ""
	}
	if len(words) == 1 {
		return words[0]
	}
	sort.Strings(words)
	commas := words[0 : len(words)-1]
	return strings.Join(commas, ", ") + " and " + words[len(words)-1]
}
