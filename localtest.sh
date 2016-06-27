BASEDIR=$(realpath $(dirname "$0"))
docker build -t droutertest -f $BASEDIR/Dockertest $BASEDIR
docker run -it --privileged --rm --pid=host -v /var/run/docker.sock:/var/run/docker.sock -v $BASEDIR/coverage:/coverage droutertest 'go test github.com/TrilliumIT/docker-drouter/drouter -coverprofile=/coverage/cover.out'
ec=$?
docker rmi droutertest > /dev/null
echo "Press enter to view coverage"
read
go tool cover -html=$BASEDIR/coverage/cover.out
exit $ec
