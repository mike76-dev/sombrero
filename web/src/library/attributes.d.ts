import 'react'

// The folder picker is an input with the webkitdirectory attribute, which
// every browser honours but the React typings do not name.
declare module 'react' {
  interface InputHTMLAttributes<T> extends HTMLAttributes<T> {
    webkitdirectory?: string
  }
}
