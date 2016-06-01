#!/bin/bash
set -e

LATEST_RELEASE=$(git describe --tags --abbrev=0 | sed "s/^v//g")
MAIN_VER=$(grep "version = " main.go | sed 's/[ \t]*version[ \t]*=[ \t]*//g' | sed 's/"//g')
VERS="${LATEST_RELEASE}\n${MAIN_VER}"

# For tagged commits
if [ "$(git describe --tags)" = "$(git describe --tags --abbrev=0)" ] ; then
	if [ $(printf ${VERS} | uniq | wc -l) -gt 1 ] ; then
		echo "This is a release, all versions should match"
		exit 1
	fi
	DKR_TAG="latest"
else
	if [ $(printf ${VERS} | uniq | wc -l) -eq 1 ] ; then
		echo "Please increment the version in main.go"
		exit 1
	fi
	if [ "$(printf ${VERS} | sort -V | tail -n 1)" != "${MAIN_VER}" ] ; then
		echo "Please increment the version in main.go"
		exit 1
	fi
	DKR_TAG="master"
fi


docker build -t trilliumit/docker-drouter:v${MAIN_VER} -t trilliumit/docker-drouter:${DKR_TAG} . 

docker run --entrypoint cat trilliumit/docker-drouter:${DKR_TAG} /go/bin/docker-drouter > docker-drouter
chmod +x docker-drouter
