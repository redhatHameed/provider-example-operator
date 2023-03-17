# Provider-Example-Operator
The GitHub repository provides a operator examples for integrating database providers with the OpenShift Database Access/DBaaS Operator. 
The examples are intended to help developers understand how to create their operator and use the operator to with DBaaS operator.

### How to create the operator

init the operator 
``` 
operator-sdk init --domain provider.com --repo github.com/redhatHameed/provider-example-operator
```

Create the API's 

```

operator-sdk create api --group dbaas.redhat.com  --version v1alpha1 --kind ProviderInventory --resource --controller

operator-sdk create api --group dbaas.redhat.com  --version v1alpha1 --kind ProviderConnection --resource --controller

operator-sdk create api --group dbaas.redhat.com  --version v1alpha1 --kind ProviderInstance --resource --controller

 ```


##  Build the operator 

```make docker-build docker-push```

```make bundle bundle-build bundle-push```

```make catalog-build catalog-push ```