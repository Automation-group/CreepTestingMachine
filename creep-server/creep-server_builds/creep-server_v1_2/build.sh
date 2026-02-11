set -e
go fmt
GOARCH=arm GOOS=linux go build
#scp ./creep-server ubuntu@creep-srv:/home/ubuntu/
#ssh ubuntu@creep-srv chmod +x /home/ubuntu/creep-server

#sudo nano /etc/rc.local
#https://www.raspberrypi.org/documentation/linux/usage/rc-local.md
