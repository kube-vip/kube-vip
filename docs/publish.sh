#!/bin/bash

echo Deploying updated documentation

generate-md --layout github --input ./index/ --output /var/www/kube-vip/
generate-md --layout github --input ./architecture/ --output /var/www/kube-vip/architecture/
generate-md --layout github --input ./control-plane/ --output /var/www/kube-vip/control-plane/
generate-md --layout github --input ./hybrid/ --output /var/www/kube-vip/hybrid/
generate-md --layout github --input ./kubernetes/ --output /var/www/kube-vip/kubernetes/
generate-md --layout github --input ./manifests/ --output /var/www/kube-vip/manifests/