# Features & roadmap

* Matrix → Steam
  * [ ] Message content
    * [x] Text
    * [x] Formatting (strip)
    * [x] Media
      * [x] Images
      * [x] Files
      * [x] Gifs
      * [x] Stickers
  * [ ] Message reactions
  * [x] Network presence/status
  * [ ] Group Membership actions
    * [ ] Join (accepting invites)
    * [ ] Invite
    * [ ] Leave
    * [ ] Kick/Ban/Unban
  * [x] Typing notifications
* Steam → Matrix
  * [x] Message content
    * [x] Text
    * [x] Media
      * [x] Images
      * [x] Gifs
      * [x] Stickers
      * [x] Steam Emoji
    * [x] Game Invites
      * [x] Invite message
      * [x] Rich invite details
      * [x] Invite acceptance
  * [ ] Network presence/status
    * [ ] Online/Offline/Away
    * [x] Rich in-game presence
  * [x] Initial profile/contact info
    * [x] Display name
    * [x] Avatar
  * [x] Profile/contact info changes
    * [x] When restarting bridge or syncing
        * [x] Display name
        * [x] Avatar
    * [x] Real time
        * [x] Display name
        * [x] Avatar
  * [x] Group info
    * [x] Name
    * [x] Avatar
    * [x] Topic
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
  * [x] Session Management
    * [x] Logout command
    * [x] Relogin command
    * [x] Automatic session expiry
  * [x] Automatic Session Recovery
    * [x] On start
    * [x] After token expiry
  * [x] Automatic portal creation
    * [x] After `start-chat`
    * [x] When receiving message
  * [ ] Private chat/group creation by inviting Matrix puppet of Steam user to new room