# Features & roadmap

* Matrix → Steam
  * [ ] Message content
    * [x] Text
    * [x] Formatting (strip)
    * [ ] Media
      * [ ] Images
      * [ ] Files
      * [ ] Gifs
      * [ ] Stickers
  * [ ] Message reactions
  * [x] Network presence/status
  * [ ] Group info changes
    * [ ] Name
    * [ ] Avatar
    * [ ] Topic
  * [ ] Group Membership actions
    * [ ] Join (accepting invites)
    * [ ] Invite
    * [ ] Leave
    * [ ] Kick/Ban/Unban
  * [ ] Group permissions
  * [ ] Typing notifications
* Steam → Matrix
  * [ ] Message content
    * [x] Text
    * [x] Media
      * [x] Images
      * [x] Gifs
      * [x] Stickers
      * [ ] Steam Emoji
    * [ ] Game Invites
      * [x] Invite message
      * [ ] Rich invite details
      * [ ] Invite acceptance
  * [ ] Network presence/status
    * [ ] Online/Offline/Away
    * [ ] Rich in-game presence
  * [x] Initial profile/contact info
    * [x] Display name
    * [x] Avatar
  * [ ] Profile/contact info changes
    * [ ] When restarting bridge or syncing
        * [ ] Display name
        * [ ] Avatar
    * [ ] Real time
        * [ ] Display name
        * [ ] Avatar
  * [ ] Group info
    * [ ] Name
    * [ ] Avatar
    * [ ] Topic
  * [ ] Membership actions
    * [ ] Join
    * [ ] Invite
    * [ ] Leave
    * [ ] Kick/Ban/Unban
  * [ ] Friend Management
    * [ ] Bridge friend requests
    * [ ] Respond to friend requests
  * [ ] Group permissions
  * [x] Typing notifications
* Misc
  * [x] Login
    * [x] Password
    * [x] SteamGuard 2FA code
    * [x] QR
    * [x] SteamGuard Prompt / E-mail
  * [ ] Session Management
    * [x] Logout command
    * [x] Relogin command
    * [ ] Automatic session expiry
  * [ ] Automatic Session Recovery
    * [x] On start
    * [ ] After token expiry
  * [x] Automatic portal creation
    * [x] After `start-chat`
    * [x] When receiving message
  * [ ] Private chat/group creation by inviting Matrix puppet of Steam user to new room