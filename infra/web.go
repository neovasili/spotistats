package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsapigatewayv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsapigatewayv2integrations"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudfront"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudfrontorigins"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsroute53"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsroute53targets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3"
	"github.com/aws/jsii-runtime-go"

	"github.com/neovasili/spotistats/internal/api"
	"github.com/neovasili/spotistats/internal/rollup"
)

// addWeb provisions the public surface: the static site bucket, the query API, and the
// CloudFront distribution that fronts both.
//
// Serving the site and the API from one distribution is what makes the whole system
// same-origin, so no CORS configuration exists anywhere (docs/SPECS.md 3.1, 7.4).
func (s *SpotistatsStack) addWeb(stack awscdk.Stack, cfg StackConfig) {
	bucket := newWebBucket(stack)
	s.WebBucket = bucket

	s.Query = s.newQueryFunction(stack, cfg)
	httpAPI := s.newHTTPAPI(stack, cfg)
	s.HTTPAPI = httpAPI

	s.Distribution = s.newDistribution(stack, cfg, bucket, httpAPI)

	// Created last: it writes to the bucket and invalidates the distribution, so both must
	// exist first.
	s.Rollup = s.newRollupFunction(stack, cfg)
	s.Enrich = s.newEnrichFunction(stack, cfg)
	s.scheduleEnrich(stack)
	s.scheduleRollup(stack, cfg)

	s.addWebOutputs(stack, cfg)
}

// newWebBucket creates the origin for the static site and the rendered snapshots.
//
// Private with no public access at all: CloudFront reads it through an Origin Access Control,
// so the bucket has exactly one reader and no object is reachable directly.
func newWebBucket(stack awscdk.Stack) awss3.Bucket {
	return awss3.NewBucket(stack, jsii.String("WebBucket"), &awss3.BucketProps{
		BlockPublicAccess: awss3.BlockPublicAccess_BLOCK_ALL(),
		Encryption:        awss3.BucketEncryption_S3_MANAGED,
		EnforceSSL:        jsii.Bool(true),
		// Versioned so a bad frontend deploy can be rolled back without a rebuild.
		Versioned: jsii.Bool(true),
		// The bucket holds only build output and rendered snapshots, both reproducible, so
		// unlike the DynamoDB table it is safe to destroy with the stack.
		RemovalPolicy:     awscdk.RemovalPolicy_DESTROY,
		AutoDeleteObjects: jsii.Bool(true),
		LifecycleRules: &[]*awss3.LifecycleRule{{
			Id:                                  jsii.String("expire-old-versions"),
			NoncurrentVersionExpiration:         awscdk.Duration_Days(jsii.Number(30)),
			AbortIncompleteMultipartUploadAfter: awscdk.Duration_Days(jsii.Number(7)),
		}},
	})
}

// newQueryFunction is the Lambda behind /api/*.
func (s *SpotistatsStack) newQueryFunction(stack awscdk.Stack, cfg StackConfig) awslambda.Function {
	logGroup := awslogs.NewLogGroup(stack, jsii.String("QueryLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/aws/lambda/spotistats-query"),
		Retention:     awslogs.RetentionDays_TWO_WEEKS,
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	fn := awslambda.NewFunction(stack, jsii.String("Query"), &awslambda.FunctionProps{
		FunctionName: jsii.String("spotistats-query"),
		Description:  jsii.String("Read-only query API behind CloudFront"),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(filepath.Join(cfg.LambdaAssetDir, "query")), nil),
		MemorySize:   jsii.Number(512),
		// Short: every endpoint is a GetItem, a BatchGetItem or one partition Query. A request
		// taking longer than this is a bug, and failing fast keeps a runaway from being billed.
		Timeout:  awscdk.Duration_Seconds(jsii.Number(10)),
		LogGroup: logGroup,
		Environment: &map[string]*string{
			"SPOTISTATS_TABLE_NAME": jsii.String(cfg.TableName),
			"SPOTISTATS_TIMEZONE":   jsii.String(cfg.Timezone),
			"SPOTISTATS_LOG_LEVEL":  jsii.String("info"),
		},
	})

	// Applied only when configured; see StackConfig.QueryReservedConcurrency. The blast radius
	// of a spike against this unauthenticated endpoint is bounded by the API Gateway stage
	// throttle regardless.
	if cfg.QueryReservedConcurrency > 0 {
		if cfnFn, ok := fn.Node().DefaultChild().(awslambda.CfnFunction); ok {
			cfnFn.SetReservedConcurrentExecutions(jsii.Number(cfg.QueryReservedConcurrency))
		}
	}

	// READ ONLY, deliberately. This is the only component reachable from the internet, so it
	// must be incapable of mutating anything -- no PutItem, no UpdateItem, no DeleteItem
	// (docs/SPECS.md 10.1). A test asserts the absence.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"dynamodb:GetItem",
			"dynamodb:BatchGetItem",
			"dynamodb:Query",
		),
		Resources: jsii.Strings(
			*s.Table.TableArn(),
			*s.Table.TableArn()+"/index/*",
		),
	}))

	return fn
}

// newHTTPAPI creates the HTTP API in front of the query Lambda.
//
// HTTP API rather than REST API: it is cheaper per request, lower latency, and none of the
// REST-only features (request validation, usage plans, WAF association) are wanted here.
func (s *SpotistatsStack) newHTTPAPI(stack awscdk.Stack, cfg StackConfig) awsapigatewayv2.HttpApi {
	integration := awsapigatewayv2integrations.NewHttpLambdaIntegration(
		jsii.String("QueryIntegration"), s.Query,
		&awsapigatewayv2integrations.HttpLambdaIntegrationProps{
			// Payload format 2.0 is what internal/api's adapter expects.
			PayloadFormatVersion: awsapigatewayv2.PayloadFormatVersion_VERSION_2_0(),
		})

	httpAPI := awsapigatewayv2.NewHttpApi(stack, jsii.String("HttpApi"), &awsapigatewayv2.HttpApiProps{
		ApiName:     jsii.String("spotistats-api"),
		Description: jsii.String("Spotistats read-only query API"),
		// No CORS configuration: production is same-origin behind CloudFront and local
		// development uses a Vite proxy (docs/SPECS.md 7.4). A CORS block appearing here
		// would mean something has gone wrong.
		DefaultIntegration: integration,
	})

	// Stage-level throttling is the first line of defence for an unauthenticated public
	// endpoint, given there is no WAF by design (docs/SPECS.md 10.3). The dashboard is served
	// as static JSON, so real API traffic is only the Explorer and these limits are generous.
	if stage := httpAPI.DefaultStage(); stage != nil {
		if cfn, ok := stage.Node().DefaultChild().(awsapigatewayv2.CfnStage); ok {
			cfn.SetDefaultRouteSettings(&awsapigatewayv2.CfnStage_RouteSettingsProperty{
				ThrottlingRateLimit:    jsii.Number(20),
				ThrottlingBurstLimit:   jsii.Number(40),
				DetailedMetricsEnabled: jsii.Bool(true),
			})
		}
	}

	_ = cfg
	return httpAPI
}

// spaRoutingFunctionCode rewrites client-side routes to the SPA entry point.
//
// docs/SPECS.md 9.1 originally specified CloudFront custom error responses for this, scoped to
// the S3 behaviours. That is not implementable: CustomErrorResponses is a DISTRIBUTION-level
// setting with no per-behaviour form, so a 404 from the API would also be rewritten to
// index.html -- exactly the confusing failure the spec was trying to avoid.
//
// A viewer-request function is the correct mechanism. It runs before the cache lookup, costs
// a fraction of a Lambda@Edge invocation, and can discriminate on the path.
const spaRoutingFunctionCode = `function handler(event) {
  var request = event.request;
  var uri = request.uri;

  // API and data paths are served by their own origins and must pass through untouched.
  if (uri.startsWith('/api/') || uri.startsWith('/data/')) {
    return request;
  }

  // Anything with a file extension is a real asset; a missing one should 404, not silently
  // render the app shell.
  var lastSegment = uri.substring(uri.lastIndexOf('/') + 1);
  if (lastSegment.indexOf('.') !== -1) {
    return request;
  }

  // Everything else is a client-side route.
  request.uri = '/index.html';
  return request;
}`

// newDistribution creates the CloudFront distribution fronting both origins.
func (s *SpotistatsStack) newDistribution(
	stack awscdk.Stack, cfg StackConfig, bucket awss3.Bucket, httpAPI awsapigatewayv2.HttpApi,
) awscloudfront.Distribution {
	// Origin Access Control, not the legacy Origin Access Identity: OAC supports SigV4 and is
	// the current mechanism.
	s3Origin := awscloudfrontorigins.S3BucketOrigin_WithOriginAccessControl(bucket,
		&awscloudfrontorigins.S3BucketOriginWithOACProps{})

	apiHost := fmt.Sprintf("%s.execute-api.%s.amazonaws.com",
		*httpAPI.ApiId(), *stack.Region())
	apiOrigin := awscloudfrontorigins.NewHttpOrigin(jsii.String(apiHost),
		&awscloudfrontorigins.HttpOriginProps{
			ProtocolPolicy: awscloudfront.OriginProtocolPolicy_HTTPS_ONLY,
			// The API is fast; a long read timeout would only hold connections open during an
			// incident.
			ReadTimeout: awscdk.Duration_Seconds(jsii.Number(15)),
		})

	spaFunction := awscloudfront.NewFunction(stack, jsii.String("SpaRouting"), &awscloudfront.FunctionProps{
		FunctionName: jsii.String("spotistats-spa-routing"),
		Comment:      jsii.String("Rewrites client-side routes to /index.html, leaving /api/* and /data/* alone"),
		Runtime:      awscloudfront.FunctionRuntime_JS_2_0(),
		Code:         awscloudfront.FunctionCode_FromInline(jsii.String(spaRoutingFunctionCode)),
	})

	securityHeaders := s.newResponseHeadersPolicy(stack)

	// The API sets its own Cache-Control (max-age=60, s-maxage=3600); this policy honours it
	// and keys the cache on the query string, which is what distinguishes one query from
	// another. Cookies and most headers are deliberately not forwarded: nothing here is
	// personalised, and forwarding them would fragment the cache.
	apiCachePolicy := awscloudfront.NewCachePolicy(stack, jsii.String("ApiCachePolicy"), &awscloudfront.CachePolicyProps{
		CachePolicyName:            jsii.String("spotistats-api"),
		Comment:                    jsii.String("Cache API responses by query string, honouring origin Cache-Control"),
		DefaultTtl:                 awscdk.Duration_Seconds(jsii.Number(60)),
		MinTtl:                     awscdk.Duration_Seconds(jsii.Number(0)),
		MaxTtl:                     awscdk.Duration_Hours(jsii.Number(1)),
		QueryStringBehavior:        awscloudfront.CacheQueryStringBehavior_All(),
		HeaderBehavior:             awscloudfront.CacheHeaderBehavior_None(),
		CookieBehavior:             awscloudfront.CacheCookieBehavior_None(),
		EnableAcceptEncodingGzip:   jsii.Bool(true),
		EnableAcceptEncodingBrotli: jsii.Bool(true),
	})

	behaviourFor := func(origin awscloudfront.IOrigin, policy awscloudfront.ICachePolicy, withSPA bool) *awscloudfront.BehaviorOptions {
		b := &awscloudfront.BehaviorOptions{
			Origin:                origin,
			ViewerProtocolPolicy:  awscloudfront.ViewerProtocolPolicy_REDIRECT_TO_HTTPS,
			AllowedMethods:        awscloudfront.AllowedMethods_ALLOW_GET_HEAD_OPTIONS(),
			CachedMethods:         awscloudfront.CachedMethods_CACHE_GET_HEAD_OPTIONS(),
			Compress:              jsii.Bool(true),
			CachePolicy:           policy,
			ResponseHeadersPolicy: securityHeaders,
		}
		if withSPA {
			b.FunctionAssociations = &[]*awscloudfront.FunctionAssociation{{
				Function:  spaFunction,
				EventType: awscloudfront.FunctionEventType_VIEWER_REQUEST,
			}}
		}
		return b
	}

	props := &awscloudfront.DistributionProps{
		Comment: jsii.String("Spotistats"),
		// The default behaviour serves the site, with SPA routing attached.
		DefaultBehavior:   behaviourFor(s3Origin, awscloudfront.CachePolicy_CACHING_OPTIMIZED(), true),
		DefaultRootObject: jsii.String("index.html"),
		AdditionalBehaviors: &map[string]*awscloudfront.BehaviorOptions{
			// The API: cached by query string for an hour at the edge.
			"/api/*": behaviourFor(apiOrigin, apiCachePolicy, false),
			// Rendered snapshots: refreshed nightly, so a short edge TTL with revalidation.
			"/data/*": behaviourFor(s3Origin, awscloudfront.CachePolicy_CACHING_OPTIMIZED(), false),
			// Hashed build assets are immutable, so cache them for a year.
			"/assets/*": behaviourFor(s3Origin, awscloudfront.CachePolicy_CACHING_OPTIMIZED(), false),
		},
		HttpVersion: awscloudfront.HttpVersion_HTTP2_AND_3,
		EnableIpv6:  jsii.Bool(true),
		// PRICE_CLASS_100 is North America and Europe. The audience is one person in Spain;
		// paying for edge locations in Asia and South America would be waste.
		PriceClass: awscloudfront.PriceClass_PRICE_CLASS_100,
	}

	// The custom domain is optional so the stack deploys before the subdomain is chosen
	// (docs/SPECS.md 14 decision 1). Without it the site is reachable on the distribution's
	// own *.cloudfront.net name, which is enough for the frontend loop and a first deploy.
	if cfg.DomainName != "" && s.certificate != nil {
		props.DomainNames = jsii.Strings(cfg.DomainName)
		props.Certificate = s.certificate
		// The TLS policy is only configurable alongside a custom certificate: on the default
		// *.cloudfront.net certificate CloudFront fixes it, so setting it there would be
		// silently ignored. TLSv1.2_2021 is the current baseline.
		props.MinimumProtocolVersion = awscloudfront.SecurityPolicyProtocol_TLS_V1_2_2021
	}

	dist := awscloudfront.NewDistribution(stack, jsii.String("Distribution"), props)

	// Alias records need a hosted zone. Without one the operator adds a CNAME by hand, which
	// docs/PREREQUISITES.md step 7 path C covers.
	if cfg.DomainName != "" && cfg.HostedZoneID != "" && cfg.HostedZoneName != "" {
		zone := awsroute53.HostedZone_FromHostedZoneAttributes(stack, jsii.String("Zone"),
			&awsroute53.HostedZoneAttributes{
				HostedZoneId: jsii.String(cfg.HostedZoneID),
				ZoneName:     jsii.String(cfg.HostedZoneName),
			})
		target := awsroute53.RecordTarget_FromAlias(awsroute53targets.NewCloudFrontTarget(dist))

		// RecordName is omitted when the domain IS the zone -- a delegated subdomain zone, as
		// here -- because CDK then creates the record at the apex. Passing the full name in
		// that case would append the zone again and produce
		// spotistats.neovasili.com.spotistats.neovasili.com.
		var recordName *string
		if !strings.EqualFold(cfg.DomainName, cfg.HostedZoneName) {
			recordName = jsii.String(cfg.DomainName)
		}

		// Both records: A for IPv4 and AAAA because the distribution has IPv6 enabled, and a
		// missing AAAA silently breaks IPv6-only clients.
		awsroute53.NewARecord(stack, jsii.String("AliasA"), &awsroute53.ARecordProps{
			Zone: zone, RecordName: recordName, Target: target,
		})
		awsroute53.NewAaaaRecord(stack, jsii.String("AliasAAAA"), &awsroute53.AaaaRecordProps{
			Zone: zone, RecordName: recordName, Target: target,
		})
	}

	return dist
}

// newResponseHeadersPolicy sets the security headers from docs/SPECS.md 9.1.
func (s *SpotistatsStack) newResponseHeadersPolicy(stack awscdk.Stack) awscloudfront.ResponseHeadersPolicy {
	// i.scdn.co must be allowlisted or every album cover and artist image breaks: Spotify
	// serves all of them from there.
	csp := "default-src 'self'; " +
		// r2.theaudiodb.com serves every fanart, banner, logo and thumbnail on the artist
		// profile. Without it the page renders image-less with only a console error, which is
		// the kind of failure nobody notices until someone opens a profile.
		"img-src 'self' https://i.scdn.co https://r2.theaudiodb.com data:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; " +
		"connect-src 'self'; " +
		"font-src 'self'; " +
		"object-src 'none'; " +
		"base-uri 'self'; " +
		"frame-ancestors 'none'"

	return awscloudfront.NewResponseHeadersPolicy(stack, jsii.String("SecurityHeaders"),
		&awscloudfront.ResponseHeadersPolicyProps{
			ResponseHeadersPolicyName: jsii.String("spotistats-security-headers"),
			SecurityHeadersBehavior: &awscloudfront.ResponseSecurityHeadersBehavior{
				ContentSecurityPolicy: &awscloudfront.ResponseHeadersContentSecurityPolicy{
					ContentSecurityPolicy: jsii.String(csp), Override: jsii.Bool(true),
				},
				ContentTypeOptions: &awscloudfront.ResponseHeadersContentTypeOptions{
					Override: jsii.Bool(true),
				},
				FrameOptions: &awscloudfront.ResponseHeadersFrameOptions{
					FrameOption: awscloudfront.HeadersFrameOption_DENY, Override: jsii.Bool(true),
				},
				ReferrerPolicy: &awscloudfront.ResponseHeadersReferrerPolicy{
					ReferrerPolicy: awscloudfront.HeadersReferrerPolicy_STRICT_ORIGIN_WHEN_CROSS_ORIGIN,
					Override:       jsii.Bool(true),
				},
				StrictTransportSecurity: &awscloudfront.ResponseHeadersStrictTransportSecurity{
					AccessControlMaxAge: awscdk.Duration_Days(jsii.Number(730)),
					IncludeSubdomains:   jsii.Bool(true),
					Preload:             jsii.Bool(true),
					Override:            jsii.Bool(true),
				},
			},
		})
}

func (s *SpotistatsStack) addWebOutputs(stack awscdk.Stack, cfg StackConfig) {
	awscdk.NewCfnOutput(stack, jsii.String("WebBucketNameOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("WebBucketName"),
		Value:       s.WebBucket.BucketName(),
		Description: jsii.String("Used by `make deploy-web`"),
	})
	awscdk.NewCfnOutput(stack, jsii.String("DistributionIdOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("DistributionId"),
		Value:       s.Distribution.DistributionId(),
		Description: jsii.String("Used by `make deploy-web` for cache invalidation"),
	})
	awscdk.NewCfnOutput(stack, jsii.String("SiteUrlOutput"), &awscdk.CfnOutputProps{
		Key: jsii.String("SiteUrl"),
		Value: jsii.String(fmt.Sprintf("https://%s",
			pick(cfg.DomainName, *s.Distribution.DistributionDomainName()))),
		Description: jsii.String("The site. Also the VITE_API_TARGET for frontend Mode A"),
	})
	awscdk.NewCfnOutput(stack, jsii.String("ApiUrlOutput"), &awscdk.CfnOutputProps{
		Key: jsii.String("ApiUrl"),
		Value: jsii.String(fmt.Sprintf("https://%s%s",
			pick(cfg.DomainName, *s.Distribution.DistributionDomainName()), api.BasePath)),
		Description: jsii.String("The query API, same-origin behind CloudFront"),
	})
}

func pick(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}

// newRollupFunction is the nightly job: reconcile, materialise, render.
func (s *SpotistatsStack) newRollupFunction(stack awscdk.Stack, cfg StackConfig) awslambda.Function {
	logGroup := awslogs.NewLogGroup(stack, jsii.String("RollupLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/aws/lambda/spotistats-rollup"),
		Retention:     awslogs.RetentionDays_TWO_WEEKS,
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	fn := awslambda.NewFunction(stack, jsii.String("Rollup"), &awslambda.FunctionProps{
		FunctionName: jsii.String("spotistats-rollup"),
		Description:  jsii.String("Nightly reconcile, leaderboards and snapshot rendering"),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(filepath.Join(cfg.LambdaAssetDir, "rollup")), nil),
		// More memory than capture: the reconcile accumulates the window's aggregates in a map,
		// and on Lambda CPU scales with memory, so this is as much about speed as space.
		MemorySize: jsii.Number(1024),
		// The full 15 minutes. A reconcile streams every play in its window and rewrites what
		// drifted; on a large library that is not quick, and being killed halfway is safe but
		// wasteful.
		Timeout:  awscdk.Duration_Minutes(jsii.Number(15)),
		LogGroup: logGroup,
		Environment: &map[string]*string{
			"SPOTISTATS_TABLE_NAME":      jsii.String(cfg.TableName),
			"SPOTISTATS_TIMEZONE":        jsii.String(cfg.Timezone),
			"SPOTISTATS_SSM_PREFIX":      jsii.String(cfg.SSMPrefix),
			"SPOTISTATS_WEB_BUCKET":      s.WebBucket.BucketName(),
			"SPOTISTATS_DISTRIBUTION_ID": s.Distribution.DistributionId(),
			"SPOTISTATS_LOG_LEVEL":       jsii.String("info"),
		},
	})

	if cfg.RollupReservedConcurrency > 0 {
		if cfnFn, ok := fn.Node().DefaultChild().(awslambda.CfnFunction); ok {
			cfnFn.SetReservedConcurrentExecutions(jsii.Number(cfg.RollupReservedConcurrency))
		}
	}

	// Full read-write on the table: this is the component that repairs aggregates. It also
	// deletes nothing, so DeleteItem is withheld.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"dynamodb:GetItem",
			"dynamodb:BatchGetItem",
			"dynamodb:PutItem",
			"dynamodb:BatchWriteItem",
			"dynamodb:UpdateItem",
			"dynamodb:Query",
		),
		Resources: jsii.Strings(
			*s.Table.TableArn(),
			*s.Table.TableArn()+"/index/*",
		),
	}))

	// Snapshots only. Scoped to the data prefix so a bug here cannot overwrite the frontend
	// bundle sitting in the same bucket.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("s3:PutObject"),
		Resources: jsii.Strings(*s.WebBucket.BucketArn() + "/" + rollup.DataPrefix + "*"),
	}))

	// CreateInvalidation cannot be scoped to a path prefix, only to the distribution.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("cloudfront:CreateInvalidation"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:cloudfront::%s:distribution/%s",
			*stack.Account(), *s.Distribution.DistributionId())),
	}))

	// Reads Spotify's own top-items rankings. Read-only: unlike capture, the rollup never
	// refreshes a token, so it has no reason to write one.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("ssm:GetParameter", "ssm:GetParameters"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s/*",
			*stack.Region(), *stack.Account(), cfg.SSMPrefix)),
	}))
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("kms:Decrypt"),
		Resources: jsii.Strings("*"),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"kms:ViaService": fmt.Sprintf("ssm.%s.amazonaws.com", *stack.Region()),
			},
		},
	}))

	return fn
}

// scheduleRollup runs the nightly job.
//
// 03:15 UTC is deliberately off the hour: every scheduled job in the world fires at :00, and
// EventBridge delivery is best-effort within a window.
func (s *SpotistatsStack) scheduleRollup(stack awscdk.Stack, cfg StackConfig) {
	rule := awsevents.NewRule(stack, jsii.String("RollupSchedule"), &awsevents.RuleProps{
		RuleName:    jsii.String("spotistats-rollup-schedule"),
		Description: jsii.String("Nightly reconcile, leaderboards and snapshot rendering"),
		Schedule: awsevents.Schedule_Cron(&awsevents.CronOptions{
			Minute: jsii.String("15"),
			Hour:   jsii.String("3"),
		}),
	})
	// One retry, unlike capture: the rollup is idempotent and a transient DynamoDB blip should
	// not leave the dashboard a day stale.
	rule.AddTarget(awseventstargets.NewLambdaFunction(s.Rollup, &awseventstargets.LambdaFunctionProps{
		RetryAttempts: jsii.Number(1),
	}))
	_ = cfg
}
