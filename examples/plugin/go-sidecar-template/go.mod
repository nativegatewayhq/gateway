module example.com/nativegateway-provider-template

go 1.25.0

require github.com/nativegatewayhq/gateway v0.0.0

// Remove this local replace and select a tagged Gateway SDK release after copying.
replace github.com/nativegatewayhq/gateway => ../../..
