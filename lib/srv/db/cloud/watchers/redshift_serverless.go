/*
Copyright 2022 Gravitational, Inc.

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

package watchers

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/redshiftserverless"
	"github.com/aws/aws-sdk-go/service/redshiftserverless/redshiftserverlessiface"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/api/types"
	libcloudaws "github.com/gravitational/teleport/lib/cloud/aws"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/db/common"
)

// redshiftServerlessFetcherConfig is the Redshift Serverless databases fetcher
// configuration.
type redshiftServerlessFetcherConfig struct {
	// Labels is a selector to match cloud databases.
	Labels types.Labels
	// Region is the AWS region to query databases in.
	Region string
	// Client is the Redshift Serverless API client.
	Client redshiftserverlessiface.RedshiftServerlessAPI
}

// CheckAndSetDefaults validates the config and sets defaults.
func (c *redshiftServerlessFetcherConfig) CheckAndSetDefaults() error {
	if len(c.Labels) == 0 {
		return trace.BadParameter("missing parameter Labels")
	}
	if c.Region == "" {
		return trace.BadParameter("missing parameter Region")
	}
	if c.Client == nil {
		return trace.BadParameter("missing parameter Client")
	}
	return nil
}

// redshiftServerlessFetcher retrieves Redshift Serverless databases.
type redshiftServerlessFetcher struct {
	cfg redshiftServerlessFetcherConfig
	log logrus.FieldLogger
}

// newRedshiftServerlessFetcher returns a new Redshift Serverless databases
// fetcher instance.
func newRedshiftServerlessFetcher(config redshiftServerlessFetcherConfig) (Fetcher, error) {
	if err := config.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	return &redshiftServerlessFetcher{
		cfg: config,
		log: logrus.WithFields(logrus.Fields{
			trace.Component: "watch:rss<", // (r)ed(s)hift (s)erver(<)less
			"labels":        config.Labels,
			"region":        config.Region,
		}),
	}, nil
}

// Get returns Redshift Serverless databases matching the watcher's selectors.
func (f *redshiftServerlessFetcher) Get(ctx context.Context) (types.Databases, error) {
	databases, workgroups, err := f.getDatabasesFromWorkgroups(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if len(workgroups) > 0 {
		vpcEndpointDatabases, err := f.getDatabasesFromVPCEndpoints(ctx, workgroups)
		if err != nil {
			if trace.IsAccessDenied(err) {
				f.log.Debugf("No permission to get Redshift Serverless VPC endpoints: %v.", err)
			} else {
				f.log.Warnf("Failed to get Redshift Serverless VPC endpoints: %v.", err)
			}
		}

		databases = append(databases, vpcEndpointDatabases...)
	}
	return filterDatabasesByLabels(databases, f.cfg.Labels, f.log), nil
}

// String returns the fetcher's string description.
func (f *redshiftServerlessFetcher) String() string {
	return fmt.Sprintf("redshiftServerlessFetcher(Region=%v, Labels=%v)", f.cfg.Region, f.cfg.Labels)
}

func (f *redshiftServerlessFetcher) getDatabasesFromWorkgroups(ctx context.Context) (types.Databases, []*redshiftserverless.Workgroup, error) {
	workgroups, err := f.getWorkgroups(ctx)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	var databases types.Databases
	for _, workgroup := range workgroups {
		if !services.IsAWSResourceAvailable(workgroup, workgroup.Status) {
			f.log.Debugf("The current status of %v is %v. Skipping.", services.ReadableAWSResourceName(workgroup), aws.StringValue(workgroup.Status))
			continue
		}

		tags, err := f.getResourceTags(ctx, workgroup.WorkgroupArn)
		if err != nil {
			if trace.IsAccessDenied(err) {
				f.log.WithError(err).Debugf("No Permission to get tags for %v.", workgroup)
			} else {
				f.log.WithError(err).Warnf("Failed to get tags for %v.", workgroup)
			}
		}

		database, err := services.NewDatabaseFromRedshiftServerlessWorkgroup(workgroup, tags)
		if err != nil {
			f.log.WithError(err).Infof("Could not convert %q to database resource.", workgroup)
			continue
		}
		databases = append(databases, database)
	}
	return databases, workgroups, nil
}

func (f *redshiftServerlessFetcher) getDatabasesFromVPCEndpoints(ctx context.Context, workgroups []*redshiftserverless.Workgroup) (types.Databases, error) {
	endpoints, err := f.getVPCEndpoints(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var databases types.Databases
	for _, endpoint := range endpoints {
		workgroup, found := findWorkgroupWithName(workgroups, aws.StringValue(endpoint.WorkgroupName))
		if !found {
			f.log.Debugf("Could not find workgroup for %v. Skipping.", services.ReadableAWSResourceName(endpoint))
			continue
		}

		if !services.IsAWSResourceAvailable(endpoint, endpoint.EndpointStatus) {
			f.log.Debugf("The current status of %v is %v. Skipping.", services.ReadableAWSResourceName(endpoint), aws.StringValue(endpoint.EndpointStatus))
			continue
		}

		tags, err := f.getResourceTags(ctx, endpoint.EndpointArn)
		if err != nil {
			if trace.IsAccessDenied(err) {
				f.log.WithError(err).Debugf("No Permission to get tags for %v.", endpoint)
			} else {
				f.log.WithError(err).Warnf("Failed to get tags for %v.", endpoint)
			}
		}

		database, err := services.NewDatabaseFromRedshiftServerlessVPCEndpoint(endpoint, workgroup, tags)
		if err != nil {
			f.log.WithError(err).Infof("Could not convert %q to database resource.", endpoint)
			continue
		}
		databases = append(databases, database)
	}
	return databases, nil
}

func (f *redshiftServerlessFetcher) getResourceTags(ctx context.Context, arn *string) ([]*redshiftserverless.Tag, error) {
	output, err := f.cfg.Client.ListTagsForResourceWithContext(ctx, &redshiftserverless.ListTagsForResourceInput{
		ResourceArn: arn,
	})
	if err != nil {
		return nil, libcloudaws.ConvertRequestFailureError(err)
	}
	return output.Tags, nil
}

func (f *redshiftServerlessFetcher) getWorkgroups(ctx context.Context) ([]*redshiftserverless.Workgroup, error) {
	var pages [][]*redshiftserverless.Workgroup
	err := f.cfg.Client.ListWorkgroupsPagesWithContext(ctx, nil, func(page *redshiftserverless.ListWorkgroupsOutput, lastPage bool) bool {
		pages = append(pages, page.Workgroups)
		return len(pages) <= common.MaxPages
	})
	return flatten(pages), libcloudaws.ConvertRequestFailureError(err)
}

func (f *redshiftServerlessFetcher) getVPCEndpoints(ctx context.Context) ([]*redshiftserverless.EndpointAccess, error) {
	var pages [][]*redshiftserverless.EndpointAccess
	err := f.cfg.Client.ListEndpointAccessPagesWithContext(ctx, nil, func(page *redshiftserverless.ListEndpointAccessOutput, lastPage bool) bool {
		pages = append(pages, page.Endpoints)
		return len(pages) <= common.MaxPages
	})
	return flatten(pages), libcloudaws.ConvertRequestFailureError(err)
}

func findWorkgroupWithName(workgroups []*redshiftserverless.Workgroup, name string) (*redshiftserverless.Workgroup, bool) {
	for _, workgroup := range workgroups {
		if aws.StringValue(workgroup.WorkgroupName) == name {
			return workgroup, true
		}
	}
	return nil, false
}

func flatten[T any](s [][]T) (result []T) {
	for i := range s {
		result = append(result, s[i]...)
	}
	return
}