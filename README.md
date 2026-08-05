# Inventory Management App
This is an inventory management APP written in Go, Api using fiber and sqLite db with fyne server GUI
It compliments the server side functionality of my another app
[complementary app](https://github.com/AnubhavSingh0708/Invenotry-Mobile)

# features
## exceptional performance using go fiber
best in class response time
![server](./screenshorts/server1.png)

## Easy setup GUI
The GUI makes it easy to set up everything on this app, accounts , tables and map. and expose ports , use certmagic to get certificates
![setup](./screenshorts/setup.png)
## detailed menus
Server Gui offer menus that would fit your every need.
![server](./screenshorts/server2.png)
## Robust detailed API
Contains a lot of detailed API that suit your demands.
## standardised maps
upload your own maps using SVG
![server](./screenshorts/server3.png)
## Optimised SQlite
For the best performance
## SSE
server side events for instant refreshes
## Advanced search API
![server](./screenshorts/server4.png)


## Included HTML pages like 
### dashboard
![dashboard](./screenshorts/dashboard.png)
### reel management UI
./public/reelmanager
![reel](./screenshorts/reel_manage.png)
### printing lables utility
![print](./screenshorts/print.png)
### QR code lookup
![print](./screenshorts/qr.png)




# known issues
## Auth safety
Currently passes keys in URI queries that may be subjected to man in the middle attacks
### planned fix
Next rollout will include Auth middleware, letting go queries for auth headers

# documentation
[docs](./docs/index.md)

### credits
Icons sourced from flaticon
