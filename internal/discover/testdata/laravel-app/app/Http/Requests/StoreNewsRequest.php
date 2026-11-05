<?php
namespace App\Http\Requests;

use Illuminate\Foundation\Http\FormRequest;

class StoreNewsRequest extends FormRequest
{
    public function rules()
    {
        return array('title' => 'required|string', 'views' => array('integer'));
    }
}
